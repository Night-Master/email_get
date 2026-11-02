package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ProxyEntry struct {
	Scheme   string
	Host     string
	Port     int
	Username string
	Password string
	Raw      string
}

type ProxySettingsView struct {
	Enabled   bool   `json:"enabled"`
	RawList   string `json:"rawList"`
	Count     int    `json:"count"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type ProxySettingsUpdate struct {
	Enabled bool   `json:"enabled"`
	RawList string `json:"rawList"`
}

type runtimeProxyState struct {
	mu       sync.Mutex
	enabled  bool
	rawList  string
	entries  []ProxyEntry
	nextItem int
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

var proxyState = &runtimeProxyState{}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func parseProxyList(raw string) ([]ProxyEntry, string, error) {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	entries := make([]ProxyEntry, 0, len(lines))
	normalized := make([]string, 0, len(lines))

	for idx, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entry, err := parseProxyLine(line)
		if err != nil {
			return nil, "", fmt.Errorf("第 %d 行代理格式错误: %w", idx+1, err)
		}
		entries = append(entries, entry)
		normalized = append(normalized, line)
	}

	return entries, strings.Join(normalized, "\n"), nil
}

func parseProxyLine(line string) (ProxyEntry, error) {
	entry := ProxyEntry{Scheme: "http", Raw: line}

	if strings.Contains(line, "://") {
		parsed, err := url.Parse(line)
		if err != nil {
			return ProxyEntry{}, err
		}
		entry.Scheme = strings.ToLower(strings.TrimSpace(parsed.Scheme))
		if entry.Scheme == "" {
			entry.Scheme = "http"
		}
		if entry.Scheme != "http" && entry.Scheme != "https" {
			return ProxyEntry{}, fmt.Errorf("仅支持 http 或 https 代理")
		}
		entry.Host = parsed.Hostname()
		port, err := parseProxyPort(parsed.Port())
		if err != nil {
			return ProxyEntry{}, err
		}
		entry.Port = port
		if parsed.User != nil {
			entry.Username = parsed.User.Username()
			entry.Password, _ = parsed.User.Password()
		}
		if entry.Host == "" {
			return ProxyEntry{}, fmt.Errorf("缺少代理主机")
		}
		return entry, nil
	}

	hostPart := line
	authPart := ""
	if at := strings.LastIndex(line, "@"); at >= 0 {
		authPart = line[:at]
		hostPart = line[at+1:]
	}

	host, portText, err := splitHostPortLoose(hostPart)
	if err != nil {
		return ProxyEntry{}, err
	}
	port, err := parseProxyPort(portText)
	if err != nil {
		return ProxyEntry{}, err
	}
	entry.Host = host
	entry.Port = port

	if authPart != "" {
		user, pass, ok := strings.Cut(authPart, ":")
		if !ok || strings.TrimSpace(user) == "" {
			return ProxyEntry{}, fmt.Errorf("认证信息必须是 user:pass 格式")
		}
		entry.Username = user
		entry.Password = pass
	}

	return entry, nil
}

func splitHostPortLoose(value string) (string, string, error) {
	host, port, err := net.SplitHostPort(value)
	if err == nil {
		return host, port, nil
	}
	idx := strings.LastIndex(value, ":")
	if idx <= 0 || idx == len(value)-1 {
		return "", "", fmt.Errorf("缺少 host:port")
	}
	return strings.TrimSpace(value[:idx]), strings.TrimSpace(value[idx+1:]), nil
}

func parseProxyPort(value string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("端口无效: %q", value)
	}
	return port, nil
}

func buildProxySettingsView(enabled bool, rawList, updatedAt string) (*ProxySettingsView, error) {
	entries, normalized, err := parseProxyList(rawList)
	if err != nil {
		return nil, err
	}
	return &ProxySettingsView{
		Enabled:   enabled,
		RawList:   normalized,
		Count:     len(entries),
		UpdatedAt: updatedAt,
	}, nil
}

func (s *runtimeProxyState) apply(enabled bool, rawList string) error {
	entries, normalized, err := parseProxyList(rawList)
	if err != nil {
		return err
	}
	if enabled && len(entries) == 0 {
		return fmt.Errorf("启用代理前至少需要配置一个有效代理")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = enabled
	s.rawList = normalized
	s.entries = entries
	if len(entries) == 0 {
		s.nextItem = 0
	} else if s.nextItem >= len(entries) {
		s.nextItem = s.nextItem % len(entries)
	}
	return nil
}

func (s *runtimeProxyState) nextProxy() *ProxyEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || len(s.entries) == 0 {
		return nil
	}
	entry := s.entries[s.nextItem%len(s.entries)]
	s.nextItem = (s.nextItem + 1) % len(s.entries)
	return &entry
}

func (p ProxyEntry) short() string {
	return net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
}

func (p ProxyEntry) proxyURL() *url.URL {
	u := &url.URL{
		Scheme: p.Scheme,
		Host:   p.short(),
	}
	if p.Username != "" || p.Password != "" {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	return u
}

func dialTCPConnection(host string, port int, timeout time.Duration) (net.Conn, error) {
	proxy := proxyState.nextProxy()
	if proxy == nil {
		return (&net.Dialer{Timeout: timeout}).Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	}
	return dialTCPViaHTTPProxy(*proxy, host, port, timeout)
}

func dialTLSConnection(host string, port int, timeout time.Duration) (net.Conn, error) {
	conn, err := dialTCPConnection(host, port, timeout)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
	})
	if err := tlsConn.SetDeadline(time.Now().Add(timeout)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := tlsConn.Handshake(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

func dialTCPViaHTTPProxy(proxy ProxyEntry, targetHost string, targetPort int, timeout time.Duration) (net.Conn, error) {
	proxyAddr := proxy.short()
	conn, err := (&net.Dialer{Timeout: timeout}).Dial("tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("代理 %s 连接失败: %w", proxyAddr, err)
	}

	if proxy.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         proxy.Host,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true,
		})
		if err := tlsConn.SetDeadline(time.Now().Add(timeout)); err != nil {
			_ = conn.Close()
			return nil, err
		}
		if err := tlsConn.Handshake(); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("代理 %s TLS 握手失败: %w", proxyAddr, err)
		}
		conn = tlsConn
	}

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		_ = conn.Close()
		return nil, err
	}

	targetAddr := net.JoinHostPort(targetHost, strconv.Itoa(targetPort))
	reader := bufio.NewReader(conn)
	var request strings.Builder
	request.WriteString("CONNECT " + targetAddr + " HTTP/1.1\r\n")
	request.WriteString("Host: " + targetAddr + "\r\n")
	if proxy.Username != "" || proxy.Password != "" {
		token := base64.StdEncoding.EncodeToString([]byte(proxy.Username + ":" + proxy.Password))
		request.WriteString("Proxy-Authorization: Basic " + token + "\r\n")
	}
	request.WriteString("Proxy-Connection: Keep-Alive\r\n\r\n")

	if _, err := conn.Write([]byte(request.String())); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("代理 %s CONNECT 请求失败: %w", proxyAddr, err)
	}

	statusLine, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("代理 %s CONNECT 响应失败: %w", proxyAddr, err)
	}
	statusLine = strings.TrimSpace(statusLine)
	if !strings.Contains(statusLine, " 200 ") {
		_ = conn.Close()
		return nil, fmt.Errorf("代理 %s CONNECT 失败: %s", proxyAddr, statusLine)
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("代理 %s CONNECT 头读取失败: %w", proxyAddr, err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	return &bufferedConn{Conn: conn, reader: reader}, nil
}

func newHTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: timeout,
	}
	if proxy := proxyState.nextProxy(); proxy != nil {
		transport.Proxy = http.ProxyURL(proxy.proxyURL())
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}
