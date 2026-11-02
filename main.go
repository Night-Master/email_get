package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/encoding/ianaindex"
)

const (
	defaultIMAPPort = 993
	defaultFolder   = "INBOX"
)

type Account struct {
	Email        string
	Password     string
	IMAPServer   string
	IMAPPort     int
	ClientID     string
	RefreshToken string
}

type Endpoint struct {
	Host     string
	Port     int
	Mode     string
	Protocol string
	Source   string
}

type ProbeReport struct {
	Domain   string
	MXHosts  []string
	IMAP     []Endpoint
	POP      []Endpoint
	SMTP     []Endpoint
	Selected *Endpoint
}

type MessageSummary struct {
	ID          int
	Folder      string
	From        string
	Subject     string
	Date        string
	Body        string
	SavedDir    string
	Attachments []string

	plainBody   string
	htmlBody    string
	rawHTMLBody string
}

type IMAPClient struct {
	conn    net.Conn
	reader  *bufio.Reader
	writer  *bufio.Writer
	tagNum  int
	timeout time.Duration
}

type POP3Client struct {
	conn    net.Conn
	reader  *bufio.Reader
	writer  *bufio.Writer
	timeout time.Duration
}

type Config struct {
	Accounts     string
	Output       string
	Folder       string
	Folders      []string
	Limit        int
	AllMessages  bool
	MarkSeen     bool
	PollInterval int
	ProbeOnly    bool
	Protocol     string
	Timeout      time.Duration
}

// resolveFolders 返回实际要抓取的文件夹列表;Web 端默认 INBOX + Junk。
func resolveFolders(cfg Config) []string {
	if len(cfg.Folders) > 0 {
		return cfg.Folders
	}
	if cfg.Folder != "" {
		return []string{cfg.Folder}
	}
	return []string{defaultFolder}
}

func legacyCLI() {
	cfg := parseFlags()
	if cfg.Limit <= 0 {
		exitf("--limit 必须大于 0")
	}
	if cfg.PollInterval < 0 {
		exitf("--poll-interval 不能小于 0")
	}
	if cfg.Protocol != "auto" && cfg.Protocol != "imap" && cfg.Protocol != "pop3" {
		exitf("--protocol 只支持 auto、imap、pop3")
	}

	if cfg.PollInterval == 0 {
		runOnce(cfg)
		return
	}

	for {
		fmt.Printf("[%s] 开始检查邮件\n", time.Now().Format("2006-01-02 15:04:05"))
		runOnce(cfg)
		fmt.Printf("等待 %d 秒后继续...\n", cfg.PollInterval)
		time.Sleep(time.Duration(cfg.PollInterval) * time.Second)
	}
}

func parseFlags() Config {
	cfg := Config{}
	flag.StringVar(&cfg.Accounts, "accounts", "accounts.txt", "账号文件路径")
	flag.StringVar(&cfg.Output, "output", "downloads_go", "邮件保存目录")
	flag.StringVar(&cfg.Folder, "folder", defaultFolder, "邮箱文件夹，默认 INBOX")
	flag.IntVar(&cfg.Limit, "limit", 10, "每个账号最多抓取多少封")
	flag.BoolVar(&cfg.AllMessages, "all-messages", false, "抓取全部邮件，不只未读")
	flag.BoolVar(&cfg.MarkSeen, "mark-seen", false, "抓取后标记为已读")
	flag.IntVar(&cfg.PollInterval, "poll-interval", 0, "轮询秒数，0 表示只跑一次")
	flag.BoolVar(&cfg.ProbeOnly, "probe-only", false, "只做 MX/IMAP/POP/SMTP 自动探测，不抓取邮件")
	flag.StringVar(&cfg.Protocol, "protocol", "auto", "收件协议: auto|imap|pop3")
	timeoutSeconds := flag.Int("timeout", 6, "网络超时秒数")
	flag.Parse()
	cfg.Protocol = normalizeProtocol(cfg.Protocol)
	cfg.Timeout = time.Duration(*timeoutSeconds) * time.Second
	return cfg
}

func runOnce(cfg Config) {
	accounts, err := loadAccounts(cfg.Accounts)
	if err != nil {
		exitf("%v", err)
	}
	if err := os.MkdirAll(cfg.Output, 0o755); err != nil {
		exitf("创建输出目录失败: %v", err)
	}

	for _, account := range accounts {
		report, endpoint, err := resolveReceiveEndpoint(account, cfg.Protocol, cfg.Timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] %s: %v\n", account.Email, err)
			continue
		}
		report.Selected = endpoint
		printProbeReport(account, report)
		if cfg.ProbeOnly {
			continue
		}
		count, err := fetchAccount(account, *endpoint, cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] %s: %v\n", account.Email, err)
			continue
		}
		fmt.Printf("[OK] %s: 保存 %d 封邮件\n", account.Email, count)
	}
}

func loadAccounts(path string) ([]Account, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	var accounts []Account
	for idx, line := range lines {
		account, err := parseAccountLine(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, idx+1, err)
		}
		if account != nil {
			accounts = append(accounts, *account)
		}
	}
	if len(accounts) == 0 {
		return nil, fmt.Errorf("没有读取到任何账号: %s", path)
	}
	return accounts, nil
}

func parseAccountLine(line string) (*Account, error) {
	raw := strings.TrimSpace(line)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return nil, nil
	}

	parts := strings.Split(raw, "----")
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			filtered = append(filtered, part)
		}
	}
	if len(filtered) < 2 {
		return nil, fmt.Errorf("账号行格式不对: %q", line)
	}

	account := &Account{
		Email:    filtered[0],
		Password: filtered[1],
		IMAPPort: defaultIMAPPort,
	}

	switch {
	case len(filtered) == 2:
		return account, nil
	case len(filtered) == 4 && looksLikeUUID(filtered[2]):
		account.ClientID = filtered[2]
		account.RefreshToken = filtered[3]
		account.IMAPServer = guessWellKnownIMAPHost(account.Email)
		if account.IMAPServer == "" {
			account.IMAPServer = "outlook.office365.com"
		}
		return account, nil
	case len(filtered) == 4:
		port, err := strconv.Atoi(filtered[3])
		if err != nil {
			return nil, fmt.Errorf("端口不是数字: %q", line)
		}
		account.IMAPServer = filtered[2]
		account.IMAPPort = port
		return account, nil
	default:
		return nil, errors.New("不支持的账号格式，请使用 `邮箱----密码`、`邮箱----密码----IMAP服务器----端口` 或 Outlook OAuth 四段格式")
	}
}

func looksLikeUUID(value string) bool {
	matched, _ := regexp.MatchString(`^[0-9a-fA-F-]{36}$`, value)
	return matched
}

func normalizeProtocol(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "auto":
		return "auto"
	case "imap":
		return "imap"
	case "pop", "pop3":
		return "pop3"
	default:
		return value
	}
}

func resolveReceiveEndpoint(account Account, protocol string, timeout time.Duration) (*ProbeReport, *Endpoint, error) {
	switch protocol {
	case "imap":
		return resolveIMAPEndpoint(account, timeout)
	case "pop3":
		return resolvePOP3Endpoint(account, timeout)
	default:
		report, endpoint, err := resolveIMAPEndpoint(account, timeout)
		if err == nil {
			return report, endpoint, nil
		}
		popReport, popEndpoint, popErr := resolvePOP3Endpoint(account, timeout)
		if popErr == nil {
			return popReport, popEndpoint, nil
		}
		return popReport, nil, fmt.Errorf("IMAP 失败: %v; POP3 失败: %v", err, popErr)
	}
}

func buildStoredEndpoint(account Account, protocol string) *Endpoint {
	if account.IMAPServer == "" {
		return nil
	}
	mode := "tls"
	if protocol == "pop3" && account.IMAPPort == 110 {
		mode = "starttls"
	}
	if protocol == "imap" && account.IMAPPort == 143 {
		mode = "starttls"
	}
	return &Endpoint{
		Host:     account.IMAPServer,
		Port:     account.IMAPPort,
		Mode:     mode,
		Protocol: protocol,
		Source:   "account",
	}
}

func requestedProtocols(protocol string) []string {
	switch normalizeProtocol(protocol) {
	case "imap":
		return []string{"imap"}
	case "pop3":
		return []string{"pop3"}
	default:
		return []string{"imap", "pop3"}
	}
}

func resolveIMAPEndpoint(account Account, timeout time.Duration) (*ProbeReport, *Endpoint, error) {
	report := &ProbeReport{Domain: emailDomain(account.Email)}
	if report.Domain != "" {
		report.MXHosts = lookupMXHosts(report.Domain)
	}
	if account.IMAPServer != "" {
		mode := "tls"
		if account.IMAPPort == 143 {
			mode = "starttls"
		}
		endpoint := &Endpoint{
			Host:     account.IMAPServer,
			Port:     account.IMAPPort,
			Mode:     mode,
			Protocol: "imap",
			Source:   "account",
		}
		if err := tryIMAPLogin(account, *endpoint, timeout); err != nil {
			return report, nil, fmt.Errorf("指定 IMAP 服务器登录失败: %w", err)
		}
		return report, endpoint, nil
	}

	candidates := buildIMAPLoginCandidates(report.Domain, report.MXHosts, guessWellKnownIMAPHost(account.Email))
	var bestErr error
	for _, endpoint := range candidates {
		if err := tryIMAPLogin(account, endpoint, timeout); err == nil {
			selected := endpoint
			probeHosts := buildProbeHosts(report.Domain, report.MXHosts, selected.Host)
			report.IMAP = probeIMAPCandidates(probeHosts, timeout)
			report.POP = probePOPCandidates(probeHosts, timeout)
			report.SMTP = probeSMTPCandidates(probeHosts, timeout)
			return report, &selected, nil
		} else {
			bestErr = preferEndpointError(bestErr, err)
		}
	}
	if bestErr != nil {
		return report, nil, fmt.Errorf("没有找到可登录的 IMAP 配置: %w", bestErr)
	}
	return report, nil, errors.New("没有找到可登录的 IMAP 配置")
}

func resolvePOP3Endpoint(account Account, timeout time.Duration) (*ProbeReport, *Endpoint, error) {
	report := &ProbeReport{Domain: emailDomain(account.Email)}
	if report.Domain != "" {
		report.MXHosts = lookupMXHosts(report.Domain)
	}
	if account.IMAPServer != "" && !(account.ClientID != "" && account.RefreshToken != "") {
		mode := "tls"
		if account.IMAPPort == 110 {
			mode = "starttls"
		}
		endpoint := &Endpoint{
			Host:     account.IMAPServer,
			Port:     account.IMAPPort,
			Mode:     mode,
			Protocol: "pop3",
			Source:   "account",
		}
		if err := tryPOP3Login(account, *endpoint, timeout); err != nil {
			return report, nil, fmt.Errorf("指定 POP3 服务器登录失败: %w", err)
		}
		return report, endpoint, nil
	}

	candidates := buildPOP3LoginCandidates(report.Domain, report.MXHosts, guessWellKnownPOPHost(account.Email))
	var bestErr error
	for _, endpoint := range candidates {
		if err := tryPOP3Login(account, endpoint, timeout); err == nil {
			selected := endpoint
			probeHosts := buildProbeHosts(report.Domain, report.MXHosts, selected.Host)
			report.IMAP = probeIMAPCandidates(probeHosts, timeout)
			report.POP = probePOPCandidates(probeHosts, timeout)
			report.SMTP = probeSMTPCandidates(probeHosts, timeout)
			return report, &selected, nil
		} else {
			bestErr = preferEndpointError(bestErr, err)
		}
	}
	if bestErr != nil {
		return report, nil, fmt.Errorf("没有找到可登录的 POP3 配置: %w", bestErr)
	}
	return report, nil, errors.New("没有找到可登录的 POP3 配置")
}

func preferEndpointError(current, next error) error {
	if next == nil {
		return current
	}
	if current == nil {
		return next
	}
	if endpointErrorScore(next) > endpointErrorScore(current) {
		return next
	}
	return current
}

func endpointErrorScore(err error) int {
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "unsafe login"):
		return 100
	case strings.Contains(text, "password error"), strings.Contains(text, "login error"), strings.Contains(text, "unable to log on"), strings.Contains(text, "authentication"):
		return 90
	case strings.Contains(text, "timeout"), strings.Contains(text, "deadline exceeded"):
		return 50
	case strings.Contains(text, "eof"), strings.Contains(text, "refused"), strings.Contains(text, "no such host"):
		return 30
	default:
		return 10
	}
}

func guessWellKnownIMAPHost(emailAddress string) string {
	parts := strings.Split(emailAddress, "@")
	if len(parts) != 2 {
		return ""
	}
	domain := strings.ToLower(parts[1])
	known := map[string]string{
		"gmail.com":   "imap.gmail.com",
		"outlook.com": "outlook.office365.com",
		"hotmail.com": "outlook.office365.com",
		"live.com":    "outlook.office365.com",
		"qq.com":      "imap.qq.com",
		"foxmail.com": "imap.qq.com",
		"163.com":     "imap.163.com",
		"126.com":     "imap.126.com",
		"yeah.net":    "imap.yeah.net",
	}
	return known[domain]
}

func guessWellKnownPOPHost(emailAddress string) string {
	parts := strings.Split(emailAddress, "@")
	if len(parts) != 2 {
		return ""
	}
	domain := strings.ToLower(parts[1])
	known := map[string]string{
		"gmail.com":   "pop.gmail.com",
		"outlook.com": "outlook.office365.com",
		"hotmail.com": "outlook.office365.com",
		"live.com":    "outlook.office365.com",
		"qq.com":      "pop.qq.com",
		"foxmail.com": "pop.qq.com",
		"163.com":     "pop.163.com",
		"126.com":     "pop.126.com",
		"yeah.net":    "pop.yeah.net",
	}
	return known[domain]
}

func probeEndpoints(emailAddress string, timeout time.Duration) *ProbeReport {
	domain := emailDomain(emailAddress)
	report := &ProbeReport{Domain: domain}
	if domain == "" {
		return report
	}

	report.MXHosts = lookupMXHosts(domain)
	candidates := candidateHosts(domain, report.MXHosts, guessWellKnownIMAPHost(emailAddress))
	report.IMAP = probeIMAPCandidates(candidates, timeout)
	report.POP = probePOPCandidates(candidates, timeout)
	report.SMTP = probeSMTPCandidates(candidates, timeout)
	return report
}

func emailDomain(emailAddress string) string {
	parts := strings.Split(emailAddress, "@")
	if len(parts) != 2 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(parts[1]))
}

func lookupMXHosts(domain string) []string {
	records, err := net.LookupMX(domain)
	if err != nil {
		return nil
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Pref == records[j].Pref {
			return records[i].Host < records[j].Host
		}
		return records[i].Pref < records[j].Pref
	})
	hosts := make([]string, 0, len(records))
	for _, record := range records {
		hosts = append(hosts, strings.TrimSuffix(strings.ToLower(record.Host), "."))
	}
	return uniqueStrings(hosts)
}

func candidateHosts(domain string, mxHosts []string, wellKnown string) []string {
	var hosts []string
	add := func(values ...string) {
		hosts = append(hosts, values...)
	}

	if wellKnown != "" {
		add(wellKnown)
	}
	add(domain,
		"imap."+domain,
		"mail."+domain,
		"mx."+domain,
		"pop."+domain,
		"pop3."+domain,
		"smtp."+domain,
		"autodiscover."+domain,
	)

	for _, mxHost := range mxHosts {
		add(mxHost)
		root := registrableSuffix(mxHost)
		if root != "" {
			add(root, "imap."+root, "mail."+root, "pop."+root, "pop3."+root, "smtp."+root)
		}
		if strings.HasPrefix(mxHost, "mail.") {
			suffix := strings.TrimPrefix(mxHost, "mail.")
			add("imap."+suffix, "pop."+suffix, "smtp."+suffix)
		}
	}

	return uniqueStrings(hosts)
}

func registrableSuffix(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return host
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(strings.ToLower(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func probeIMAPCandidates(hosts []string, timeout time.Duration) []Endpoint {
	var specs []Endpoint
	for _, host := range hosts {
		specs = append(specs,
			Endpoint{Host: host, Port: 993, Mode: "tls", Protocol: "imap", Source: "probe"},
			Endpoint{Host: host, Port: 143, Mode: "starttls", Protocol: "imap", Source: "probe"},
		)
	}
	return concurrentProbe(specs, func(endpoint Endpoint) bool {
		if endpoint.Port == 993 {
			return probeTLSEndpoint(endpoint.Host, endpoint.Port, timeout, "*")
		}
		return probePlainBanner(endpoint.Host, endpoint.Port, timeout, "*")
	})
}

func probePOPCandidates(hosts []string, timeout time.Duration) []Endpoint {
	var specs []Endpoint
	for _, host := range hosts {
		specs = append(specs,
			Endpoint{Host: host, Port: 995, Mode: "tls", Protocol: "pop3", Source: "probe"},
			Endpoint{Host: host, Port: 110, Mode: "starttls", Protocol: "pop3", Source: "probe"},
		)
	}
	return concurrentProbe(specs, func(endpoint Endpoint) bool {
		if endpoint.Port == 995 {
			return probeTLSEndpoint(endpoint.Host, endpoint.Port, timeout, "+OK")
		}
		return probePlainBanner(endpoint.Host, endpoint.Port, timeout, "+OK")
	})
}

func probeSMTPCandidates(hosts []string, timeout time.Duration) []Endpoint {
	var specs []Endpoint
	for _, host := range hosts {
		specs = append(specs,
			Endpoint{Host: host, Port: 465, Mode: "tls", Protocol: "smtp", Source: "probe"},
			Endpoint{Host: host, Port: 587, Mode: "starttls", Protocol: "smtp", Source: "probe"},
			Endpoint{Host: host, Port: 25, Mode: "starttls", Protocol: "smtp", Source: "probe"},
		)
	}
	return concurrentProbe(specs, func(endpoint Endpoint) bool {
		if endpoint.Port == 465 {
			return probeTLSEndpoint(endpoint.Host, endpoint.Port, timeout, "220")
		}
		return probePlainBanner(endpoint.Host, endpoint.Port, timeout, "220")
	})
}

func concurrentProbe(specs []Endpoint, probe func(Endpoint) bool) []Endpoint {
	results := make(chan Endpoint, len(specs))
	limit := make(chan struct{}, 16)
	var wg sync.WaitGroup

	for _, spec := range specs {
		spec := spec
		wg.Add(1)
		go func() {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			if probe(spec) {
				results <- spec
			}
		}()
	}

	wg.Wait()
	close(results)

	var endpoints []Endpoint
	for endpoint := range results {
		endpoints = append(endpoints, endpoint)
	}
	return rankEndpoints(endpoints)
}

func probeTLSEndpoint(host string, port int, timeout time.Duration, expectPrefix string) bool {
	conn, err := dialTLSConnection(host, port, timeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(line), expectPrefix)
}

func probePlainBanner(host string, port int, timeout time.Duration, expectPrefix string) bool {
	conn, err := dialTCPConnection(host, port, timeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(line), expectPrefix)
}

func rankEndpoints(endpoints []Endpoint) []Endpoint {
	seen := map[string]struct{}{}
	var unique []Endpoint
	for _, endpoint := range endpoints {
		key := fmt.Sprintf("%s|%d|%s|%s", endpoint.Host, endpoint.Port, endpoint.Mode, endpoint.Protocol)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, endpoint)
	}
	sort.Slice(unique, func(i, j int) bool {
		return endpointScore(unique[i]) > endpointScore(unique[j])
	})
	return unique
}

func endpointScore(endpoint Endpoint) int {
	score := 0
	switch endpoint.Protocol {
	case "imap":
		score += 100
	case "pop3":
		score += 60
	case "smtp":
		score += 30
	}
	switch endpoint.Port {
	case 993, 995, 587:
		score += 20
	case 465:
		score += 15
	case 143, 110:
		score += 10
	case 25:
		score += 5
	}
	if endpoint.Mode == "tls" {
		score += 10
	}
	if strings.HasPrefix(endpoint.Host, "imap.") {
		score += 8
	}
	if strings.HasPrefix(endpoint.Host, "pop.") {
		score += 7
	}
	if strings.HasPrefix(endpoint.Host, "mail.") {
		score += 6
	}
	return score
}

func prependUniqueEndpoint(endpoints []Endpoint, endpoint Endpoint) []Endpoint {
	key := fmt.Sprintf("%s|%d|%s|%s", endpoint.Host, endpoint.Port, endpoint.Mode, endpoint.Protocol)
	var filtered []Endpoint
	filtered = append(filtered, endpoint)
	for _, item := range endpoints {
		itemKey := fmt.Sprintf("%s|%d|%s|%s", item.Host, item.Port, item.Mode, item.Protocol)
		if itemKey == key {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func buildIMAPLoginCandidates(domain string, mxHosts []string, wellKnown string) []Endpoint {
	hosts := candidateHosts(domain, mxHosts, wellKnown)
	var endpoints []Endpoint
	for _, host := range hosts {
		endpoints = append(endpoints,
			Endpoint{Host: host, Port: 993, Mode: "tls", Protocol: "imap", Source: "autodetect"},
			Endpoint{Host: host, Port: 143, Mode: "starttls", Protocol: "imap", Source: "autodetect"},
		)
	}
	return rankEndpoints(endpoints)
}

func buildPOP3LoginCandidates(domain string, mxHosts []string, wellKnown string) []Endpoint {
	hosts := candidateHosts(domain, mxHosts, wellKnown)
	var endpoints []Endpoint
	for _, host := range hosts {
		endpoints = append(endpoints,
			Endpoint{Host: host, Port: 995, Mode: "tls", Protocol: "pop3", Source: "autodetect"},
			Endpoint{Host: host, Port: 110, Mode: "starttls", Protocol: "pop3", Source: "autodetect"},
		)
	}
	return rankEndpoints(endpoints)
}

func buildProbeHosts(domain string, mxHosts []string, selectedHost string) []string {
	hosts := []string{selectedHost, domain}
	hosts = append(hosts, mxHosts...)
	for _, mxHost := range mxHosts {
		root := registrableSuffix(mxHost)
		if root != "" {
			hosts = append(hosts, root, "imap."+root, "mail."+root, "pop."+root, "smtp."+root)
		}
		if strings.HasPrefix(mxHost, "mail.") {
			suffix := strings.TrimPrefix(mxHost, "mail.")
			hosts = append(hosts, "imap."+suffix, "pop."+suffix, "smtp."+suffix)
		}
	}
	return uniqueStrings(hosts)
}

func printProbeReport(account Account, report *ProbeReport) {
	if report == nil {
		return
	}
	mxSummary := "-"
	if len(report.MXHosts) > 0 {
		mxSummary = strings.Join(report.MXHosts, ", ")
	}
	fmt.Printf("[PROBE] %s domain=%s mx=%s\n", account.Email, report.Domain, mxSummary)
	fmt.Printf("        imap=%s\n", summarizeEndpoints(report.IMAP))
	fmt.Printf("        pop=%s\n", summarizeEndpoints(report.POP))
	fmt.Printf("        smtp=%s\n", summarizeEndpoints(report.SMTP))
	if report.Selected != nil {
		fmt.Printf("        selected=%s:%d (%s/%s)\n", report.Selected.Host, report.Selected.Port, report.Selected.Protocol, report.Selected.Mode)
	}
}

func summarizeEndpoints(endpoints []Endpoint) string {
	if len(endpoints) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		parts = append(parts, fmt.Sprintf("%s:%d/%s", endpoint.Host, endpoint.Port, endpoint.Mode))
	}
	return strings.Join(parts, ", ")
}

func tryIMAPLogin(account Account, endpoint Endpoint, timeout time.Duration) error {
	client, err := dialIMAP(endpoint, timeout)
	if err != nil {
		return err
	}
	defer client.Logout()

	if account.ClientID != "" && account.RefreshToken != "" {
		token, err := requestOutlookAccessToken(account.ClientID, account.RefreshToken, outlookScope("imap"), timeout)
		if err != nil {
			return err
		}
		if err := client.AuthXOAUTH2(account.Email, token); err != nil {
			return err
		}
	} else {
		if err := client.Login(account.Email, account.Password); err != nil {
			return err
		}
	}
	_, err = client.Select(defaultFolder)
	return err
}

func tryPOP3Login(account Account, endpoint Endpoint, timeout time.Duration) error {
	client, err := dialPOP3(endpoint, timeout)
	if err != nil {
		return err
	}
	defer client.Quit()

	if account.ClientID != "" && account.RefreshToken != "" {
		token, err := requestOutlookAccessToken(account.ClientID, account.RefreshToken, outlookScope("pop3"), timeout)
		if err != nil {
			return err
		}
		if err := client.AuthXOAUTH2(account.Email, token); err != nil {
			return err
		}
	} else {
		if err := client.Login(account.Email, account.Password); err != nil {
			return err
		}
	}
	return nil
}

func tryEndpointLogin(account Account, endpoint Endpoint, timeout time.Duration) error {
	switch endpoint.Protocol {
	case "pop3":
		return tryPOP3Login(account, endpoint, timeout)
	default:
		return tryIMAPLogin(account, endpoint, timeout)
	}
}

func humanizeProtocolError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	switch {
	case strings.Contains(text, "Unsafe Login"):
		return "IMAP 被邮箱服务商判定为风险登录，已阻止打开收件箱；建议改用 POP3 或等待系统自动回退"
	case strings.Contains(text, "LOGIN Login error or password error"):
		return "登录失败：密码或授权码错误"
	case strings.Contains(text, "Unable to log on"):
		return "登录失败：密码或授权码错误"
	case strings.Contains(strings.ToLower(text), "i/o timeout"):
		return "连接超时"
	default:
		return text
	}
}

func fetchPOP3Account(account Account, endpoint Endpoint, cfg Config) (int, error) {
	messages, err := fetchPOP3AccountDetailed(account, endpoint, cfg)
	if err != nil {
		return 0, err
	}
	return len(messages), nil
}

func fetchPOP3AccountDetailed(account Account, endpoint Endpoint, cfg Config) ([]*MessageSummary, error) {
	client, err := dialPOP3(endpoint, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	defer client.Quit()

	if account.ClientID != "" && account.RefreshToken != "" {
		token, err := requestOutlookAccessToken(account.ClientID, account.RefreshToken, outlookScope("pop3"), cfg.Timeout)
		if err != nil {
			return nil, err
		}
		if err := client.AuthXOAUTH2(account.Email, token); err != nil {
			return nil, err
		}
	} else {
		if err := client.Login(account.Email, account.Password); err != nil {
			return nil, err
		}
	}

	ids, err := client.List()
	if err != nil {
		return nil, err
	}
	if len(ids) > cfg.Limit {
		ids = ids[len(ids)-cfg.Limit:]
	}

	accountDir := ""
	if strings.TrimSpace(cfg.Output) != "" {
		accountDir = filepath.Join(cfg.Output, sanitizeFilename(account.Email))
		if err := os.MkdirAll(accountDir, 0o755); err != nil {
			return nil, err
		}
	}

	var messages []*MessageSummary
	for _, id := range ids {
		rawMessage, err := client.Retrieve(id)
		if err != nil {
			continue
		}
		summary, err := saveMessage(accountDir, account.Email, id, rawMessage)
		if err != nil {
			continue
		}
		printMessagePreview(account.Email, summary)
		messages = append(messages, summary)
	}
	return messages, nil
}

func fetchAccount(account Account, endpoint Endpoint, cfg Config) (int, error) {
	messages, err := fetchAccountDetailed(account, endpoint, cfg)
	if err != nil {
		return 0, err
	}
	return len(messages), nil
}

func fetchAccountDetailed(account Account, endpoint Endpoint, cfg Config) ([]*MessageSummary, error) {
	if endpoint.Protocol == "pop3" {
		return fetchPOP3AccountDetailed(account, endpoint, cfg)
	}

	client, err := dialIMAP(endpoint, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	defer client.Logout()

	if account.ClientID != "" && account.RefreshToken != "" {
		token, err := requestOutlookAccessToken(account.ClientID, account.RefreshToken, outlookScope("imap"), cfg.Timeout)
		if err != nil {
			return nil, err
		}
		if err := client.AuthXOAUTH2(account.Email, token); err != nil {
			return nil, err
		}
	} else {
		if err := client.Login(account.Email, account.Password); err != nil {
			return nil, err
		}
	}

	accountDir := ""
	if strings.TrimSpace(cfg.Output) != "" {
		accountDir = filepath.Join(cfg.Output, sanitizeFilename(account.Email))
		if err := os.MkdirAll(accountDir, 0o755); err != nil {
			return nil, err
		}
	}

	criterion := "UNSEEN"
	if cfg.AllMessages {
		criterion = "ALL"
	}

	var messages []*MessageSummary
	var folderErrs []string
	for _, folder := range resolveFolders(cfg) {
		if _, err := client.Select(folder); err != nil {
			// 文件夹不存在(如有的邮箱没有 Junk)不算致命,记录后继续
			folderErrs = append(folderErrs, fmt.Sprintf("%s: %s", folder, humanizeProtocolError(err)))
			continue
		}
		ids, err := client.Search(criterion)
		if err != nil {
			folderErrs = append(folderErrs, fmt.Sprintf("%s: %s", folder, humanizeProtocolError(err)))
			continue
		}
		if len(ids) > cfg.Limit {
			ids = ids[len(ids)-cfg.Limit:]
		}
		for _, id := range ids {
			rawMessage, err := client.FetchMessage(id, !cfg.MarkSeen)
			if err != nil {
				folderErrs = append(folderErrs, fmt.Sprintf("%s 邮件 #%d: %s", folder, id, humanizeProtocolError(err)))
				continue
			}
			summary, err := saveMessage(accountDir, account.Email, id, rawMessage)
			if err != nil {
				folderErrs = append(folderErrs, fmt.Sprintf("%s 邮件 #%d: %s", folder, id, err))
				continue
			}
			summary.Folder = folder
			printMessagePreview(account.Email, summary)
			messages = append(messages, summary)
		}
	}

	// 一封都没抓到且有错误时,把错误抛出去而不是静默返回空列表
	if len(messages) == 0 && len(folderErrs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(folderErrs, "; "))
	}
	return messages, nil
}

func dialPOP3(endpoint Endpoint, timeout time.Duration) (*POP3Client, error) {
	var conn net.Conn
	var err error
	if endpoint.Mode == "tls" {
		conn, err = dialTLSConnection(endpoint.Host, endpoint.Port, timeout)
	} else {
		conn, err = dialTCPConnection(endpoint.Host, endpoint.Port, timeout)
	}
	if err != nil {
		return nil, err
	}

	client := &POP3Client{
		conn:    conn,
		reader:  bufio.NewReader(conn),
		writer:  bufio.NewWriter(conn),
		timeout: timeout,
	}
	if err := client.setDeadline(timeout); err != nil {
		conn.Close()
		return nil, err
	}
	line, err := client.readLine()
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.HasPrefix(line, "+OK") {
		conn.Close()
		return nil, fmt.Errorf("POP3 欢迎语异常: %s", line)
	}
	if endpoint.Mode == "starttls" {
		if err := client.STLS(endpoint.Host, timeout); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return client, nil
}

func (c *POP3Client) setDeadline(timeout time.Duration) error {
	return c.conn.SetDeadline(time.Now().Add(timeout))
}

func (c *POP3Client) readLine() (string, error) {
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *POP3Client) command(command string) (string, error) {
	if c.timeout > 0 {
		_ = c.setDeadline(c.timeout)
	}
	if _, err := c.writer.WriteString(command + "\r\n"); err != nil {
		return "", err
	}
	if err := c.writer.Flush(); err != nil {
		return "", err
	}
	line, err := c.readLine()
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(line, "+OK") {
		return line, errors.New(line)
	}
	return line, nil
}

func (c *POP3Client) multiline(command string) ([]string, error) {
	if c.timeout > 0 {
		_ = c.setDeadline(c.timeout)
	}
	if _, err := c.writer.WriteString(command + "\r\n"); err != nil {
		return nil, err
	}
	if err := c.writer.Flush(); err != nil {
		return nil, err
	}
	line, err := c.readLine()
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "+OK") {
		return nil, errors.New(line)
	}
	var lines []string
	for {
		item, err := c.readLine()
		if err != nil {
			return nil, err
		}
		if item == "." {
			return lines, nil
		}
		if strings.HasPrefix(item, "..") {
			item = item[1:]
		}
		lines = append(lines, item)
	}
}

func (c *POP3Client) STLS(host string, timeout time.Duration) error {
	if _, err := c.command("STLS"); err != nil {
		return err
	}
	tlsConn := tls.Client(c.conn, &tls.Config{
		ServerName:         host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
	})
	if err := tlsConn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	if err := tlsConn.Handshake(); err != nil {
		return err
	}
	c.conn = tlsConn
	c.reader = bufio.NewReader(tlsConn)
	c.writer = bufio.NewWriter(tlsConn)
	return nil
}

func (c *POP3Client) Login(username, password string) error {
	if _, err := c.command("USER " + username); err != nil {
		return err
	}
	_, err := c.command("PASS " + password)
	return err
}

func (c *POP3Client) AuthXOAUTH2(username, token string) error {
	if _, err := c.writer.WriteString("AUTH XOAUTH2\r\n"); err != nil {
		return err
	}
	if err := c.writer.Flush(); err != nil {
		return err
	}
	line, err := c.readLine()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, "+") {
		return errors.New(line)
	}
	payload := fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", username, token)
	encoded := base64.StdEncoding.EncodeToString([]byte(payload))
	if _, err := c.writer.WriteString(encoded + "\r\n"); err != nil {
		return err
	}
	if err := c.writer.Flush(); err != nil {
		return err
	}
	line, err = c.readLine()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, "+OK") {
		return errors.New(line)
	}
	return nil
}

func (c *POP3Client) List() ([]int, error) {
	lines, err := c.multiline("LIST")
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		id, err := strconv.Atoi(fields[0])
		if err == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (c *POP3Client) Retrieve(id int) ([]byte, error) {
	if _, err := c.writer.WriteString(fmt.Sprintf("RETR %d\r\n", id)); err != nil {
		return nil, err
	}
	if err := c.writer.Flush(); err != nil {
		return nil, err
	}
	line, err := c.readLine()
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "+OK") {
		return nil, errors.New(line)
	}

	var buf bytes.Buffer
	for {
		item, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimRight(item, "\r\n")
		if trimmed == "." {
			return buf.Bytes(), nil
		}
		if strings.HasPrefix(trimmed, "..") {
			trimmed = trimmed[1:]
		}
		buf.WriteString(trimmed)
		buf.WriteString("\r\n")
	}
}

func (c *POP3Client) Quit() {
	if c == nil || c.conn == nil {
		return
	}
	_, _ = c.command("QUIT")
	_ = c.conn.Close()
}

func dialIMAP(endpoint Endpoint, timeout time.Duration) (*IMAPClient, error) {
	var conn net.Conn
	var err error
	if endpoint.Mode == "tls" {
		conn, err = dialTLSConnection(endpoint.Host, endpoint.Port, timeout)
	} else {
		conn, err = dialTCPConnection(endpoint.Host, endpoint.Port, timeout)
	}
	if err != nil {
		return nil, err
	}

	client := &IMAPClient{
		conn:    conn,
		reader:  bufio.NewReader(conn),
		writer:  bufio.NewWriter(conn),
		timeout: timeout,
	}

	if err := client.setDeadline(timeout); err != nil {
		conn.Close()
		return nil, err
	}
	line, err := client.readLine()
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		conn.Close()
		return nil, fmt.Errorf("IMAP 欢迎语异常: %s", line)
	}

	if endpoint.Mode == "starttls" {
		if err := client.StartTLS(endpoint.Host, timeout); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return client, nil
}

func (c *IMAPClient) setDeadline(timeout time.Duration) error {
	return c.conn.SetDeadline(time.Now().Add(timeout))
}

func (c *IMAPClient) readLine() (string, error) {
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *IMAPClient) nextTag() string {
	c.tagNum++
	return fmt.Sprintf("A%04d", c.tagNum)
}

func (c *IMAPClient) writeCommand(format string, args ...any) (string, error) {
	// 每条命令前刷新 deadline,超时按单条命令计而不是按整条连接计,
	// 避免大邮件 FETCH 因连接存活时间耗尽而超时。
	if c.timeout > 0 {
		_ = c.setDeadline(c.timeout)
	}
	tag := c.nextTag()
	command := fmt.Sprintf(format, args...)
	if _, err := c.writer.WriteString(tag + " " + command + "\r\n"); err != nil {
		return "", err
	}
	if err := c.writer.Flush(); err != nil {
		return "", err
	}
	return tag, nil
}

func (c *IMAPClient) readTagged(tag string) ([]string, error) {
	var lines []string
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
		if strings.HasPrefix(line, tag+" ") {
			if strings.Contains(line, " OK") {
				return lines, nil
			}
			return lines, errors.New(line)
		}
	}
}

func (c *IMAPClient) StartTLS(host string, timeout time.Duration) error {
	tag, err := c.writeCommand("STARTTLS")
	if err != nil {
		return err
	}
	if _, err := c.readTagged(tag); err != nil {
		return err
	}
	tlsConn := tls.Client(c.conn, &tls.Config{
		ServerName:         host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
	})
	if err := tlsConn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	if err := tlsConn.Handshake(); err != nil {
		return err
	}
	c.conn = tlsConn
	c.reader = bufio.NewReader(tlsConn)
	c.writer = bufio.NewWriter(tlsConn)
	return nil
}

func (c *IMAPClient) Login(username, password string) error {
	tag, err := c.writeCommand("LOGIN %s %s", imapString(username), imapString(password))
	if err != nil {
		return err
	}
	_, err = c.readTagged(tag)
	return err
}

func (c *IMAPClient) AuthXOAUTH2(username, token string) error {
	payload := fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", username, token)
	encoded := base64.StdEncoding.EncodeToString([]byte(payload))
	tag, err := c.writeCommand("AUTHENTICATE XOAUTH2 %s", encoded)
	if err != nil {
		return err
	}
	_, err = c.readTagged(tag)
	return err
}

func (c *IMAPClient) Select(folder string) (int, error) {
	tag, err := c.writeCommand("SELECT %s", imapString(folder))
	if err != nil {
		return 0, err
	}
	lines, err := c.readTagged(tag)
	if err != nil {
		return 0, err
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "* ") && strings.Contains(line, " EXISTS") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				count, _ := strconv.Atoi(fields[1])
				return count, nil
			}
		}
	}
	return 0, nil
}

func (c *IMAPClient) Search(criteria string) ([]int, error) {
	tag, err := c.writeCommand("SEARCH %s", criteria)
	if err != nil {
		return nil, err
	}
	lines, err := c.readTagged(tag)
	if err != nil {
		return nil, err
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "* SEARCH") {
			fields := strings.Fields(line)
			var ids []int
			for _, field := range fields[2:] {
				id, err := strconv.Atoi(field)
				if err == nil {
					ids = append(ids, id)
				}
			}
			return ids, nil
		}
	}
	return nil, nil
}

func (c *IMAPClient) FetchMessage(id int, peek bool) ([]byte, error) {
	item := "RFC822"
	if peek {
		item = "BODY.PEEK[]"
	}
	tag, err := c.writeCommand("FETCH %d %s", id, item)
	if err != nil {
		return nil, err
	}

	var message []byte
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(line, tag+" ") {
			if strings.Contains(line, " OK") {
				return message, nil
			}
			return nil, errors.New(line)
		}
		if length, ok := parseLiteralLength(line); ok {
			buf := make([]byte, length)
			if _, err := io.ReadFull(c.reader, buf); err != nil {
				return nil, err
			}
			message = append(message, buf...)
			if _, err := c.readLine(); err != nil {
				return nil, err
			}
		}
	}
}

func (c *IMAPClient) Logout() {
	if c == nil || c.conn == nil {
		return
	}
	tag, err := c.writeCommand("LOGOUT")
	if err == nil {
		_, _ = c.readTagged(tag)
	}
	_ = c.conn.Close()
}

func imapString(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(value) + `"`
}

func parseLiteralLength(line string) (int, bool) {
	start := strings.LastIndex(line, "{")
	end := strings.LastIndex(line, "}")
	if start == -1 || end == -1 || end <= start+1 {
		return 0, false
	}
	length, err := strconv.Atoi(line[start+1 : end])
	if err != nil {
		return 0, false
	}
	return length, true
}

func requestOutlookAccessToken(clientID, refreshToken, scope string, timeout time.Duration) (string, error) {
	endpoints := []string{
		"https://login.microsoftonline.com/consumers/oauth2/v2.0/token",
		"https://login.microsoftonline.com/common/oauth2/v2.0/token",
	}
	httpClient := newHTTPClient(timeout)
	var lastErr error

	for _, endpoint := range endpoints {
		form := url.Values{}
		form.Set("client_id", clientID)
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", refreshToken)
		form.Set("scope", scope)

		req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
			continue
		}

		var payload struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			lastErr = err
			continue
		}
		if payload.AccessToken != "" {
			return payload.AccessToken, nil
		}
		lastErr = errors.New("响应里没有 access_token")
	}

	return "", lastErr
}

func outlookScope(protocol string) string {
	switch protocol {
	case "pop3":
		return "https://outlook.office.com/POP.AccessAsUser.All offline_access"
	default:
		return "https://outlook.office.com/IMAP.AccessAsUser.All offline_access"
	}
}

func saveMessage(outputDir, accountEmail string, id int, raw []byte) (*MessageSummary, error) {
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}

	subject := decodeHeader(message.Header.Get("Subject"))
	if subject == "" {
		subject = "no_subject"
	}
	summary := &MessageSummary{
		ID:      id,
		From:    decodeHeader(message.Header.Get("From")),
		Subject: subject,
		Date:    decodeHeader(message.Header.Get("Date")),
	}

	bodyBytes, err := io.ReadAll(message.Body)
	if err != nil {
		return nil, err
	}

	messageDir := ""
	if outputDir != "" {
		messageDir = filepath.Join(outputDir, fmt.Sprintf("%d_%s", id, sanitizeFilename(subject)))
		if err := os.MkdirAll(messageDir, 0o755); err != nil {
			return nil, err
		}
		summary.SavedDir = messageDir
	}

	contentType := message.Header.Get("Content-Type")
	if err := extractEntity(contentType, message.Header, bodyBytes, messageDir, summary); err != nil {
		return nil, err
	}
	if summary.htmlBody != "" {
		summary.Body = summary.htmlBody
	} else {
		summary.Body = summary.plainBody
	}

	if messageDir != "" {
		content := strings.TrimSpace(strings.Join([]string{
			"Account: " + accountEmail,
			"From: " + summary.From,
			"Subject: " + summary.Subject,
			"Date: " + summary.Date,
			"",
			summary.Body,
		}, "\n"))
		if err := os.WriteFile(filepath.Join(messageDir, "message.txt"), []byte(content+"\n"), 0o644); err != nil {
			return nil, err
		}
	}

	return summary, nil
}

func extractEntity(contentType string, header mail.Header, body []byte, targetDir string, summary *MessageSummary) error {
	body = decodeTransferEncoding(header, body)

	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return nil
		}
		reader := multipart.NewReader(bytes.NewReader(body), boundary)
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			partBody, err := io.ReadAll(part)
			part.Close()
			if err != nil {
				return err
			}
			partHeader := mail.Header{}
			for key, values := range part.Header {
				if len(values) > 0 {
					partHeader[key] = append([]string(nil), values...)
				}
			}
			disposition, dispParams, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			filename := decodeHeader(part.FileName())
			if filename == "" {
				filename = decodeHeader(dispParams["filename"])
			}
			if disposition == "attachment" || filename != "" {
				name := sanitizeFilename(filename)
				if name == "" {
					name = "attachment.bin"
				}
				if targetDir == "" {
					summary.Attachments = append(summary.Attachments, name)
					continue
				}
				filePath := filepath.Join(targetDir, name)
				if err := os.WriteFile(filePath, partBody, 0o644); err != nil {
					return err
				}
				summary.Attachments = append(summary.Attachments, filePath)
				continue
			}
			if err := extractEntity(part.Header.Get("Content-Type"), partHeader, partBody, targetDir, summary); err != nil {
				return err
			}
		}
	}

	switch mediaType {
	case "text/plain":
		if summary.plainBody == "" {
			summary.plainBody = decodeBody(decodeCharset(params["charset"], body))
		}
	case "text/html":
		decoded := decodeCharset(params["charset"], body)
		if summary.htmlBody == "" {
			summary.htmlBody = htmlToText(decodeBody(decoded))
		}
		if summary.rawHTMLBody == "" {
			summary.rawHTMLBody = strings.TrimSpace(string(decoded))
		}
	}
	return nil
}

// decodeCharset 按 Content-Type 声明的 charset 把正文转成 UTF-8;未声明或不识别时原样返回。
func decodeCharset(charset string, body []byte) []byte {
	name := strings.ToLower(strings.TrimSpace(charset))
	if name == "" || name == "utf-8" || name == "utf8" || name == "us-ascii" {
		return body
	}
	enc, err := ianaindex.IANA.Encoding(name)
	if err != nil || enc == nil {
		return body
	}
	decoded, err := enc.NewDecoder().Bytes(body)
	if err != nil {
		return body
	}
	return decoded
}

func decodeHeader(value string) string {
	if value == "" {
		return ""
	}
	decoder := mime.WordDecoder{}
	decoded, err := decoder.DecodeHeader(value)
	if err != nil {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(decoded)
}

func decodeBody(body []byte) string {
	if utf8Text := strings.TrimSpace(string(body)); utf8Text != "" {
		return utf8Text
	}
	return strings.TrimSpace(string(body))
}

func decodeTransferEncoding(header mail.Header, body []byte) []byte {
	encoding := strings.ToLower(strings.TrimSpace(header.Get("Content-Transfer-Encoding")))
	switch encoding {
	case "base64":
		decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(body)))
		if err == nil {
			return decoded
		}
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err == nil {
			return decoded
		}
	}
	return body
}

func htmlToText(value string) string {
	value = regexp.MustCompile(`(?is)<!--.*?-->`).ReplaceAllString(value, "")
	value = regexp.MustCompile(`(?is)<(script|style).*?>.*?</(script|style)>`).ReplaceAllString(value, "")
	value = regexp.MustCompile(`(?is)<img\b[^>]*>`).ReplaceAllStringFunc(value, func(tag string) string {
		sub := regexp.MustCompile(`(?is)\salt\s*=\s*(?:"([^"]*)"|'([^']*)')`).FindStringSubmatch(tag)
		if sub == nil {
			return ""
		}
		alt := strings.TrimSpace(html.UnescapeString(sub[1] + sub[2]))
		if alt == "" {
			return ""
		}
		return " [" + alt + "] "
	})
	value = regexp.MustCompile(`(?i)<br\s*/?>`).ReplaceAllString(value, "\n")
	value = regexp.MustCompile(`(?i)</(p|div|tr|table|ul|ol|h[1-6])>`).ReplaceAllString(value, "\n")
	value = regexp.MustCompile(`(?i)</t[dh]\s*>`).ReplaceAllString(value, " | ")
	value = regexp.MustCompile(`(?i)<li[^>]*>`).ReplaceAllString(value, "\n")
	linkRe := regexp.MustCompile(`(?is)<a\b[^>]*?\shref\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))[^>]*>(.*?)</a>`)
	value = linkRe.ReplaceAllStringFunc(value, func(m string) string {
		sub := linkRe.FindStringSubmatch(m)
		href := sub[1] + sub[2] + sub[3]
		text := strings.TrimSpace(regexp.MustCompile(`(?s)<.*?>`).ReplaceAllString(sub[4], ""))
		if text == "" {
			return href
		}
		if href == "" {
			return text
		}
		return text + " (" + href + ")"
	})
	value = regexp.MustCompile(`(?s)<.*?>`).ReplaceAllString(value, "")
	value = html.UnescapeString(value)
	value = regexp.MustCompile(`[ \t]+\n`).ReplaceAllString(value, "\n")
	value = regexp.MustCompile(`[ \t]{2,}`).ReplaceAllString(value, " ")
	value = regexp.MustCompile(`\n{3,}`).ReplaceAllString(value, "\n\n")
	return strings.TrimSpace(value)
}

func sanitizeFilename(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "mail"
	}
	reg := regexp.MustCompile(`[^\w.\-]+`)
	value = reg.ReplaceAllString(value, "_")
	if len(value) > 120 {
		value = value[:120]
	}
	return value
}

func printMessagePreview(accountEmail string, summary *MessageSummary) {
	preview := strings.Join(strings.Fields(summary.Body), " ")
	if len(preview) > 200 {
		preview = preview[:200]
	}
	if preview == "" {
		preview = "-"
	}
	fmt.Printf("[MAIL] %s id=%d from=%s\n", accountEmail, summary.ID, blankDash(summary.From))
	fmt.Printf("       subject=%s\n", blankDash(summary.Subject))
	fmt.Printf("       date=%s\n", blankDash(summary.Date))
	fmt.Printf("       body=%s\n", preview)
}

func blankDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
