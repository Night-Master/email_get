package main

import (
	"crypto/rand"
	"crypto/sha1"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/mail"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed all:web/dist
var embeddedWeb embed.FS

//go:embed API.md
var apiDocsMarkdown string

// defaultFetchFolders Web 端默认收件文件夹:INBOX + Junk(验证码常被判进垃圾邮件)
var defaultFetchFolders = []string{"INBOX", "Junk"}

// defaultFetchTimeout 单条 IMAP/POP3 命令的默认超时;大邮件 FETCH 8 秒不够
const defaultFetchTimeout = 30 * time.Second

type WebApp struct {
	mu                sync.RWMutex
	fetchJobsMu       sync.RWMutex
	fetchJobs         map[string]*FetchJob
	fetchJobOrder     []string
	activeFetchOwners map[string]string
	state             AppState
	db                *sql.DB
	statePath         string
	dataDir           string
	fetchTimeout      time.Duration
}

type AppState struct {
	Accounts  []StoredAccount `json:"accounts"`
	Messages  []StoredMessage `json:"messages"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

type StoredAccount struct {
	ID            string     `json:"id"`
	Email         string     `json:"email"`
	Password      string     `json:"password"`
	Server        string     `json:"server,omitempty"`
	Port          int        `json:"port,omitempty"`
	ClientID      string     `json:"clientId,omitempty"`
	RefreshToken  string     `json:"refreshToken,omitempty"`
	Protocol      string     `json:"protocol"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	LastFetchedAt *time.Time `json:"lastFetchedAt,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
	LastProtocol  string     `json:"lastProtocol,omitempty"`
}

type AccountView struct {
	ID            string     `json:"id"`
	Email         string     `json:"email"`
	Server        string     `json:"server,omitempty"`
	Port          int        `json:"port,omitempty"`
	Protocol      string     `json:"protocol"`
	HasOAuth      bool       `json:"hasOAuth"`
	HasServer     bool       `json:"hasServer"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	LastFetchedAt *time.Time `json:"lastFetchedAt,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
	LastProtocol  string     `json:"lastProtocol,omitempty"`
}

type StoredMessage struct {
	ID           string    `json:"id"`
	AccountID    string    `json:"accountId"`
	AccountEmail string    `json:"accountEmail"`
	RemoteID     int       `json:"remoteId"`
	Folder       string    `json:"folder,omitempty"`
	Protocol     string    `json:"protocol"`
	From         string    `json:"from"`
	Subject      string    `json:"subject"`
	Date         string    `json:"date"`
	Body         string    `json:"body"`
	HTMLBody     string    `json:"htmlBody,omitempty"`
	Preview      string    `json:"preview"`
	SavedDir     string    `json:"savedDir"`
	Attachments  []string  `json:"attachments"`
	FetchedAt    time.Time `json:"fetchedAt"`
}

type ImportAccountsRequest struct {
	Text     string `json:"text"`
	Protocol string `json:"protocol"`
}

type FetchRequest struct {
	AccountIDs  []string `json:"accountIds"`
	Emails      []string `json:"emails"`
	Limit       int      `json:"limit"`
	AllMessages bool     `json:"allMessages"`
	MarkSeen    bool     `json:"markSeen"`
	Async       bool     `json:"async,omitempty"`
	Folders     []string `json:"folders,omitempty"`
}

type FetchAccountResult struct {
	AccountID string `json:"accountId"`
	Email     string `json:"email"`
	Protocol  string `json:"protocol,omitempty"`
	Count     int    `json:"count"`
	Error     string `json:"error,omitempty"`
}

type FetchResponse struct {
	Results []FetchAccountResult `json:"results"`
}

type FetchJobResponse struct {
	Job     *FetchJob         `json:"job,omitempty"`
	Jobs    []*FetchJob       `json:"jobs,omitempty"`
	Started bool              `json:"started,omitempty"`
	Message string            `json:"message,omitempty"`
	Skipped []FetchJobAccount `json:"skipped,omitempty"`
}

type SearchRequest struct {
	AccountIDs    []string `json:"accountIds"`
	Emails        []string `json:"emails"`
	RecentSeconds int64    `json:"recentSeconds"`
	Keyword       string   `json:"keyword"`
	Regex         string   `json:"regex"`
	Fields        []string `json:"fields"`
	Limit         int      `json:"limit"`
	ScanLimit     int      `json:"scanLimit"`
	Folders       []string `json:"folders,omitempty"`
}

type SearchResponse struct {
	Messages []StoredMessage `json:"messages"`
}

type proxySettingsResponse struct {
	Settings *ProxySettingsView `json:"settings"`
}

func NewWebApp(dataDir string) (*WebApp, error) {
	dataDir = filepath.Clean(dataDir)
	app := &WebApp{
		dataDir:           dataDir,
		statePath:         filepath.Join(dataDir, "state.json"),
		fetchJobs:         map[string]*FetchJob{},
		activeFetchOwners: map[string]string{},
		fetchTimeout:      defaultFetchTimeout,
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	if err := app.initDB(); err != nil {
		return nil, err
	}
	if err := app.migrateFromJSONIfNeeded(); err != nil {
		return nil, err
	}
	if err := app.loadStateFromDB(); err != nil {
		return nil, err
	}
	if err := app.loadProxySettings(); err != nil {
		return nil, err
	}
	return app, nil
}

func (a *WebApp) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", a.handleHealth)
	mux.HandleFunc("/api/auth/login", a.handleAuthLogin)
	mux.HandleFunc("/api/auth/logout", a.handleAuthLogout)
	mux.HandleFunc("/login", a.handleLoginPage)
	mux.HandleFunc("/api/accounts", a.handleAccounts)
	mux.HandleFunc("/api/accounts/import", a.handleImportAccounts)
	mux.HandleFunc("/api/accounts/", a.handleAccountByID)
	mux.HandleFunc("/api/messages", a.handleMessages)
	mux.HandleFunc("/api/messages/", a.handleMessageByID)
	mux.HandleFunc("/api/fetch", a.handleFetch)
	mux.HandleFunc("/api/fetch/", a.handleFetchByID)
	mux.HandleFunc("/api/search", a.handleSearch)
	mux.HandleFunc("/api/endpoints", a.handleEndpoints)
	mux.HandleFunc("/api/endpoints/", a.handleEndpointByID)
	mux.HandleFunc("/api/proxy-settings", a.handleProxySettings)
	mux.HandleFunc("/api/docs", a.handleDocs)
	mux.HandleFunc("/docs", a.handleDocs)
	mux.Handle("/", a.staticHandler())
	return a.authMiddleware(gzipMiddleware(mux))
}

func (a *WebApp) staticHandler() http.Handler {
	sub, err := fs.Sub(embeddedWeb, "web/dist")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		})
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/files/") {
			http.NotFound(w, r)
			return
		}
		cleanPath := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		// assets 下的文件名带内容 hash,可永久缓存;HTML 不缓存。
		if strings.HasPrefix(cleanPath, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if cleanPath == "." || cleanPath == "" {
			r.URL.Path = "/"
			fileServer.ServeHTTP(w, r)
			return
		}
		if _, err := fs.Stat(sub, cleanPath); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		indexData, readErr := fs.ReadFile(sub, "index.html")
		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexData)
	})
}

func (a *WebApp) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDocs 输出 API 文档(markdown 源码用 <pre> 直出,免引入渲染依赖)。
// 路由在 authMiddleware 之后,未登录会被 302/401。
func (a *WebApp) handleDocs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/api/docs" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(apiDocsMarkdown))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8" />
<meta name="viewport" content="width=device-width, initial-scale=1.0" />
<title>API 文档 - Email Get</title>
<style>
  body { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
         max-width: 900px; margin: 0 auto; padding: 24px; background: #f6f8fa; }
  pre { white-space: pre-wrap; word-break: break-word; background: #fff;
        padding: 24px; border-radius: 8px; border: 1px solid #e1e4e8; line-height: 1.6; }
</style>
</head>
<body><pre>` + htmlEscapeString(apiDocsMarkdown) + `</pre></body>
</html>`))
}

func htmlEscapeString(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func (a *WebApp) handleProxySettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		view, err := a.getProxySettingsView()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, proxySettingsResponse{Settings: view})
	case http.MethodPut:
		var req ProxySettingsUpdate
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		view, err := a.saveProxySettings(req.Enabled, req.RawList)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, proxySettingsResponse{Settings: view})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *WebApp) handleAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		keyword := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("keyword")))
		a.mu.RLock()
		views := make([]AccountView, 0, len(a.state.Accounts))
		for _, account := range a.state.Accounts {
			if keyword != "" {
				haystack := strings.ToLower(strings.Join([]string{
					account.Email,
					account.Server,
					account.ClientID,
				}, " "))
				if !strings.Contains(haystack, keyword) {
					continue
				}
			}
			views = append(views, toAccountView(account))
		}
		a.mu.RUnlock()
		sort.Slice(views, func(i, j int) bool {
			return views[i].CreatedAt.After(views[j].CreatedAt)
		})
		writeJSON(w, http.StatusOK, map[string]any{"accounts": views})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *WebApp) handleImportAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req ImportAccountsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Protocol = normalizeProtocol(req.Protocol)
	if req.Protocol != "auto" && req.Protocol != "imap" && req.Protocol != "pop3" {
		req.Protocol = "auto"
	}

	added, updated, err := a.importAccounts(req.Text, req.Protocol)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"added":   added,
		"updated": updated,
	})
}

func (a *WebApp) handleAccountByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if err := a.deleteAccount(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (a *WebApp) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	accountID := strings.TrimSpace(r.URL.Query().Get("accountId"))
	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	a.mu.RLock()
	messages := make([]StoredMessage, 0, len(a.state.Messages))
	for _, message := range a.state.Messages {
		if accountID != "" && message.AccountID != accountID {
			continue
		}
		messages = append(messages, message)
	}
	a.mu.RUnlock()

	sort.Slice(messages, func(i, j int) bool {
		left, right := messageSortTime(messages[i]), messageSortTime(messages[j])
		if !left.Equal(right) {
			return left.After(right)
		}
		return messages[i].FetchedAt.After(messages[j].FetchedAt)
	})
	if limit > 0 && len(messages) > limit {
		messages = messages[:limit]
	}
	for i := range messages {
		messages[i].Date = chinaDateText(messages[i].Date)
		messages[i].HTMLBody = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
}

func (a *WebApp) handleMessageByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/messages/")
	if id == "" {
		http.NotFound(w, r)
		return
	}

	if r.Method == http.MethodGet {
		a.mu.RLock()
		var message *StoredMessage
		for i := range a.state.Messages {
			if a.state.Messages[i].ID == id {
				copy := a.state.Messages[i]
				message = &copy
				break
			}
		}
		a.mu.RUnlock()
		if message == nil {
			http.NotFound(w, r)
			return
		}
		message.Date = chinaDateText(message.Date)
		writeJSON(w, http.StatusOK, map[string]any{"message": message})
		return
	}

	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := a.deleteMessage(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": true,
		"scope":   "local_db_only",
	})
}

func (a *WebApp) handleFetch(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jobs := a.activeFetchJobs()
		resp := FetchJobResponse{Jobs: jobs}
		if len(jobs) > 0 {
			resp.Job = jobs[0]
		}
		writeJSON(w, http.StatusOK, resp)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req FetchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Limit <= 0 {
		req.Limit = 20
	}
	if len(req.Folders) == 0 {
		req.Folders = defaultFetchFolders
	}
	if req.Async {
		resp, status, err := a.startFetchJob(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, status, resp)
		return
	}

	resp, err := a.fetchAccounts(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *WebApp) handleFetchByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/fetch/"))
	if id == "" {
		http.NotFound(w, r)
		return
	}
	job := a.getFetchJob(id)
	if job == nil {
		http.Error(w, "收件任务不存在", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, FetchJobResponse{Job: job})
}

func (a *WebApp) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req SearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Limit <= 0 {
		req.Limit = 20
	}
	if req.RecentSeconds <= 0 {
		req.RecentSeconds = 86400
	}
	if req.ScanLimit <= 0 {
		req.ScanLimit = 20
	}
	if req.ScanLimit < req.Limit {
		req.ScanLimit = req.Limit
	}
	if len(req.Folders) == 0 {
		req.Folders = defaultFetchFolders
	}

	resp, err := a.searchAccounts(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *WebApp) importAccounts(text, defaultProtocol string) (int, int, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	now := time.Now()
	added := 0
	updated := 0

	a.mu.Lock()
	defer a.mu.Unlock()

	for idx, line := range lines {
		account, err := parseAccountLine(line)
		if err != nil {
			return added, updated, fmt.Errorf("第 %d 行: %w", idx+1, err)
		}
		if account == nil {
			continue
		}

		index := findAccountIndex(a.state.Accounts, *account)
		if index >= 0 {
			record := a.state.Accounts[index]
			record.Email = account.Email
			record.Password = account.Password
			record.Server = account.IMAPServer
			record.Port = account.IMAPPort
			record.ClientID = account.ClientID
			record.RefreshToken = account.RefreshToken
			record.Protocol = defaultProtocol
			record.UpdatedAt = now
			a.state.Accounts[index] = record
			updated++
			continue
		}

		record := StoredAccount{
			ID:           randomID(),
			Email:        account.Email,
			Password:     account.Password,
			Server:       account.IMAPServer,
			Port:         account.IMAPPort,
			ClientID:     account.ClientID,
			RefreshToken: account.RefreshToken,
			Protocol:     defaultProtocol,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		a.state.Accounts = append(a.state.Accounts, record)
		added++
	}
	a.state.UpdatedAt = now
	if err := a.saveStateToDB(); err != nil {
		return added, updated, err
	}
	return added, updated, nil
}

func (a *WebApp) deleteAccount(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	index := -1
	for i, account := range a.state.Accounts {
		if account.ID == id {
			index = i
			break
		}
	}
	if index == -1 {
		return fmt.Errorf("账号不存在: %s", id)
	}
	a.state.Accounts = append(a.state.Accounts[:index], a.state.Accounts[index+1:]...)

	filtered := a.state.Messages[:0]
	for _, message := range a.state.Messages {
		if message.AccountID != id {
			filtered = append(filtered, message)
		}
	}
	a.state.Messages = filtered
	a.state.UpdatedAt = time.Now()
	return a.saveStateToDB()
}

func (a *WebApp) deleteMessage(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	index := -1
	for i, message := range a.state.Messages {
		if message.ID == id {
			index = i
			break
		}
	}
	if index == -1 {
		return fmt.Errorf("邮件不存在: %s", id)
	}
	a.state.Messages = append(a.state.Messages[:index], a.state.Messages[index+1:]...)
	a.state.UpdatedAt = time.Now()
	return a.saveStateToDB()
}

func (a *WebApp) fetchAccounts(req FetchRequest) (*FetchResponse, error) {
	selectedAccounts := a.selectedAccounts(req)
	results := a.fetchSelectedAccounts(req, selectedAccounts, nil, nil)
	return &FetchResponse{Results: results}, nil
}

func (a *WebApp) selectedAccounts(req FetchRequest) []StoredAccount {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return selectAccounts(a.state.Accounts, req.AccountIDs, req.Emails)
}

func (a *WebApp) fetchSelectedAccounts(req FetchRequest, selectedAccounts []StoredAccount, beforeEach func(StoredAccount), afterEach func(StoredAccount, FetchAccountResult)) []FetchAccountResult {
	results := make([]FetchAccountResult, 0, len(selectedAccounts))
	for _, stored := range selectedAccounts {
		if beforeEach != nil {
			beforeEach(stored)
		}
		result := a.fetchSingleAccount(stored, req)
		results = append(results, result)
		if afterEach != nil {
			afterEach(stored, result)
		}
	}
	return results
}

func (a *WebApp) fetchSingleAccount(stored StoredAccount, req FetchRequest) FetchAccountResult {
	result := FetchAccountResult{
		AccountID: stored.ID,
		Email:     stored.Email,
	}

	runtimeAccount := Account{
		Email:        stored.Email,
		Password:     stored.Password,
		IMAPServer:   stored.Server,
		IMAPPort:     stored.Port,
		ClientID:     stored.ClientID,
		RefreshToken: stored.RefreshToken,
	}

	report, endpoint, err := a.resolveReceiveEndpoint(runtimeAccount, normalizeProtocol(stored.Protocol), a.fetchTimeout)
	if err != nil {
		result.Error = err.Error()
		a.updateAccountFetchResult(stored.ID, "", err)
		return result
	}
	report.Selected = endpoint
	result.Protocol = endpoint.Protocol

	cfg := Config{
		Output:      "",
		Folders:     req.Folders,
		Limit:       req.Limit,
		AllMessages: req.AllMessages,
		MarkSeen:    req.MarkSeen,
		Protocol:    normalizeProtocol(stored.Protocol),
		Timeout:     a.fetchTimeout,
	}
	summaries, err := fetchAccountDetailed(runtimeAccount, *endpoint, cfg)
	if err != nil {
		result.Error = err.Error()
		a.updateAccountFetchResult(stored.ID, endpoint.Protocol, err)
		return result
	}
	a.storeFetchedMessages(stored, endpoint.Protocol, summaries)
	result.Count = len(summaries)
	return result
}

func (a *WebApp) resolveReceiveEndpoint(account Account, protocol string, timeout time.Duration) (*ProbeReport, *Endpoint, error) {
	// 明确指定协议时，server/port 才视为固定协议配置。
	// auto 模式不能因为账号上带了 server 就被锁死在 IMAP。
	if account.IMAPServer != "" && protocol != "auto" {
		endpoint := buildStoredEndpoint(account, normalizeProtocol(protocol))
		if endpoint != nil {
			if err := tryEndpointLogin(account, *endpoint, timeout); err != nil {
				return nil, nil, fmt.Errorf("%s 失败: %s", strings.ToUpper(endpoint.Protocol), humanizeProtocolError(err))
			}
			domain := emailDomain(account.Email)
			if domain != "" {
				_ = a.saveCachedEndpoint(domain, normalizeProtocol(protocol), *endpoint)
			}
			return &ProbeReport{Selected: endpoint}, endpoint, nil
		}
	}

	domain := emailDomain(account.Email)
	report := &ProbeReport{Domain: domain}

	for _, requested := range requestedProtocols(protocol) {
		cached, err := a.loadCachedEndpoint(domain, requested)
		if err != nil {
			break
		}
		if cached == nil {
			continue
		}
		if err := tryEndpointLogin(account, *cached, timeout); err == nil {
			report.Selected = cached
			return report, cached, nil
		}
	}

	// 对已知邮箱服务优先走官方主机，避免 auto 模式下把一串候选 IMAP 全部试完才回退。
	if protocol == "auto" {
		var preferredErr error
		triedPreferred := false
		if preferredIMAP := guessWellKnownIMAPHost(account.Email); preferredIMAP != "" {
			triedPreferred = true
			endpoint := &Endpoint{Host: preferredIMAP, Port: 993, Mode: "tls", Protocol: "imap", Source: "builtin"}
			if err := tryEndpointLogin(account, *endpoint, timeout); err == nil {
				report.Selected = endpoint
				if domain != "" {
					_ = a.saveCachedEndpoint(domain, "imap", *endpoint)
				}
				return report, endpoint, nil
			} else {
				preferredErr = preferEndpointError(preferredErr, fmt.Errorf("IMAP 失败: %w", err))
			}
		}
		if preferredPOP3 := guessWellKnownPOPHost(account.Email); preferredPOP3 != "" {
			triedPreferred = true
			endpoint := &Endpoint{Host: preferredPOP3, Port: 995, Mode: "tls", Protocol: "pop3", Source: "builtin"}
			if err := tryEndpointLogin(account, *endpoint, timeout); err == nil {
				report.Selected = endpoint
				if domain != "" {
					_ = a.saveCachedEndpoint(domain, "pop3", *endpoint)
				}
				return report, endpoint, nil
			} else {
				preferredErr = preferEndpointError(preferredErr, fmt.Errorf("POP3 失败: %w", err))
			}
		}
		if triedPreferred && preferredErr != nil && endpointErrorScore(preferredErr) >= 90 {
			return report, nil, fmt.Errorf("%s", humanizeProtocolError(preferredErr))
		}
	}

	// auto 模式下如果账号记录里自带了 server/port，就先把它作为候选尝试，
	// 失败后仍然继续走完整的 auto 回退链路。
	if account.IMAPServer != "" && protocol == "auto" {
		if storedIMAP := buildStoredEndpoint(account, "imap"); storedIMAP != nil {
			if err := tryEndpointLogin(account, *storedIMAP, timeout); err == nil {
				report.Selected = storedIMAP
				if domain != "" {
					_ = a.saveCachedEndpoint(domain, "imap", *storedIMAP)
				}
				return report, storedIMAP, nil
			}
		}
		if storedPOP3 := buildStoredEndpoint(account, "pop3"); storedPOP3 != nil {
			if err := tryEndpointLogin(account, *storedPOP3, timeout); err == nil {
				report.Selected = storedPOP3
				if domain != "" {
					_ = a.saveCachedEndpoint(domain, "pop3", *storedPOP3)
				}
				return report, storedPOP3, nil
			}
		}
	}

	finalReport, endpoint, err := resolveReceiveEndpoint(account, protocol, timeout)
	if err != nil {
		return finalReport, nil, fmt.Errorf("%s", humanizeProtocolError(err))
	}
	if domain != "" && endpoint != nil {
		_ = a.saveCachedEndpoint(domain, endpoint.Protocol, *endpoint)
	}
	return finalReport, endpoint, nil
}

func (a *WebApp) updateAccountFetchResult(accountID, protocol string, fetchErr error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	a.updateAccountFetchResultLocked(accountID, protocol, fetchErr, now)
	a.state.UpdatedAt = now
	if err := a.saveStateToDB(); err != nil {
		fmt.Printf("[DB ERROR] updateAccountFetchResult: %v\n", err)
	}
}

func (a *WebApp) updateAccountFetchResultLocked(accountID, protocol string, fetchErr error, now time.Time) {
	for i, account := range a.state.Accounts {
		if account.ID != accountID {
			continue
		}
		account.UpdatedAt = now
		account.LastProtocol = protocol
		if fetchErr != nil {
			account.LastError = humanizeProtocolError(fetchErr)
		} else {
			account.LastError = ""
			account.LastFetchedAt = &now
		}
		a.state.Accounts[i] = account
		break
	}
}

func (a *WebApp) upsertMessages(account StoredAccount, protocol string, summaries []*MessageSummary) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	added := a.upsertMessagesLocked(account, protocol, summaries, now)
	a.state.UpdatedAt = now
	if err := a.saveStateToDB(); err != nil {
		fmt.Printf("[DB ERROR] upsertMessages: %v\n", err)
	}
	return added
}

func (a *WebApp) upsertMessagesLocked(account StoredAccount, protocol string, summaries []*MessageSummary, now time.Time) int {
	if len(summaries) == 0 {
		return 0
	}

	added := 0
	for _, summary := range summaries {
		signature := messageSignature(account.ID, summary)
		record := StoredMessage{
			ID:           signature,
			AccountID:    account.ID,
			AccountEmail: account.Email,
			RemoteID:     summary.ID,
			Folder:       summary.Folder,
			Protocol:     protocol,
			From:         summary.From,
			Subject:      summary.Subject,
			Date:         summary.Date,
			Body:         summary.Body,
			HTMLBody:     summary.rawHTMLBody,
			Preview:      previewText(summary.Body),
			SavedDir:     "",
			Attachments:  append([]string(nil), summary.Attachments...),
			FetchedAt:    now,
		}

		index := -1
		for i, existing := range a.state.Messages {
			if existing.ID == signature {
				index = i
				break
			}
		}
		if index >= 0 {
			record.FetchedAt = a.state.Messages[index].FetchedAt
			a.state.Messages[index] = record
			continue
		}
		a.state.Messages = append(a.state.Messages, record)
		added++
	}

	sort.Slice(a.state.Messages, func(i, j int) bool {
		return a.state.Messages[i].FetchedAt.After(a.state.Messages[j].FetchedAt)
	})
	a.state.Messages = dedupeMessages(a.state.Messages)
	if len(a.state.Messages) > 500 {
		a.state.Messages = a.state.Messages[:500]
	}
	return added
}

func (a *WebApp) storeFetchedMessages(account StoredAccount, protocol string, summaries []*MessageSummary) {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	a.upsertMessagesLocked(account, protocol, summaries, now)
	a.updateAccountFetchResultLocked(account.ID, protocol, nil, now)
	a.state.UpdatedAt = now
	if err := a.saveStateToDB(); err != nil {
		fmt.Printf("[DB ERROR] storeFetchedMessages: %v\n", err)
	}
}

func findAccountIndex(accounts []StoredAccount, target Account) int {
	for i, account := range accounts {
		if account.Email == target.Email &&
			account.Server == target.IMAPServer &&
			account.Port == target.IMAPPort &&
			account.ClientID == target.ClientID {
			return i
		}
	}
	return -1
}

func selectAccounts(accounts []StoredAccount, accountIDs []string, emails []string) []StoredAccount {
	if len(accountIDs) == 0 && len(emails) == 0 {
		return append([]StoredAccount(nil), accounts...)
	}

	idSet := map[string]struct{}{}
	emailSet := map[string]struct{}{}
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			idSet[id] = struct{}{}
		}
	}
	for _, email := range emails {
		email = strings.ToLower(strings.TrimSpace(email))
		if email != "" {
			emailSet[email] = struct{}{}
		}
	}

	selected := make([]StoredAccount, 0, len(accounts))
	for _, account := range accounts {
		if _, ok := idSet[account.ID]; ok {
			selected = append(selected, account)
			continue
		}
		if _, ok := emailSet[strings.ToLower(account.Email)]; ok {
			selected = append(selected, account)
		}
	}
	return selected
}

func toAccountView(account StoredAccount) AccountView {
	return AccountView{
		ID:            account.ID,
		Email:         account.Email,
		Server:        account.Server,
		Port:          account.Port,
		Protocol:      normalizeProtocol(account.Protocol),
		HasOAuth:      account.ClientID != "" && account.RefreshToken != "",
		HasServer:     account.Server != "",
		CreatedAt:     account.CreatedAt,
		UpdatedAt:     account.UpdatedAt,
		LastFetchedAt: account.LastFetchedAt,
		LastError:     account.LastError,
		LastProtocol:  account.LastProtocol,
	}
}

func messageSignature(accountID string, summary *MessageSummary) string {
	sum := sha1.Sum([]byte(strings.Join([]string{
		accountID,
		summary.Folder,
		strconv.Itoa(summary.ID),
		summary.From,
		summary.Subject,
		summary.Date,
	}, "|")))
	return hex.EncodeToString(sum[:])
}

func dedupeMessages(messages []StoredMessage) []StoredMessage {
	indexes := map[string]int{}
	var deduped []StoredMessage
	for _, message := range messages {
		key := strings.Join([]string{
			message.AccountID,
			strconv.Itoa(message.RemoteID),
			message.From,
			message.Subject,
			message.Date,
		}, "|")
		if idx, ok := indexes[key]; ok {
			if message.FetchedAt.After(deduped[idx].FetchedAt) {
				deduped[idx] = message
			}
			continue
		}
		indexes[key] = len(deduped)
		deduped = append(deduped, message)
	}
	sort.Slice(deduped, func(i, j int) bool {
		return deduped[i].FetchedAt.After(deduped[j].FetchedAt)
	})
	return deduped
}

func previewText(body string) string {
	preview := strings.Join(strings.Fields(body), " ")
	if len(preview) > 240 {
		preview = preview[:240]
	}
	return preview
}

var chinaZone = time.FixedZone("CST", 8*3600)

// chinaDateText 把邮件 Date 头解析后转换为中国时区显示;解析失败时保留原文。
func chinaDateText(value string) string {
	parsed, err := mail.ParseDate(value)
	if err != nil {
		return value
	}
	return parsed.In(chinaZone).Format("2006-01-02 15:04:05")
}

// messageSortTime 优先用邮件 Date 头的发送时间排序,解析失败退回收取时间。
func messageSortTime(message StoredMessage) time.Time {
	if parsed := parseRemoteMailDate(message.Date); !parsed.IsZero() {
		return parsed
	}
	return message.FetchedAt
}

func relativeTo(base, full string) string {
	if full == "" {
		return ""
	}
	rel, err := filepath.Rel(base, full)
	if err != nil {
		return full
	}
	return filepath.ToSlash(rel)
}

func relativeManyTo(base string, items []string) []string {
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, relativeTo(base, item))
	}
	return result
}

func randomID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

type EndpointCacheView struct {
	Domain            string `json:"domain"`
	RequestedProtocol string `json:"requestedProtocol"`
	ActualProtocol    string `json:"actualProtocol"`
	Host              string `json:"host"`
	Port              int    `json:"port"`
	Mode              string `json:"mode"`
	UpdatedAt         string `json:"updatedAt"`
}

type EndpointCacheUpdate struct {
	ActualProtocol string `json:"actualProtocol"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Mode           string `json:"mode"`
}

func (a *WebApp) handleEndpoints(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := a.db.Query(`SELECT domain, requested_protocol, actual_protocol, host, port, mode, updated_at FROM endpoint_cache ORDER BY domain, requested_protocol`)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var endpoints []EndpointCacheView
		for rows.Next() {
			var e EndpointCacheView
			if err := rows.Scan(&e.Domain, &e.RequestedProtocol, &e.ActualProtocol, &e.Host, &e.Port, &e.Mode, &e.UpdatedAt); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			endpoints = append(endpoints, e)
		}

		writeJSON(w, http.StatusOK, map[string]any{"endpoints": endpoints})

	case http.MethodPost:
		var e EndpointCacheView
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if e.Domain == "" || e.ActualProtocol == "" || e.Host == "" || e.Port == 0 {
			http.Error(w, "domain, actualProtocol, host, port are required", http.StatusBadRequest)
			return
		}
		if e.RequestedProtocol == "" {
			e.RequestedProtocol = e.ActualProtocol
		}
		if e.Mode == "" {
			e.Mode = "tls"
		}
		_, err := a.db.Exec(
			`INSERT INTO endpoint_cache (domain, requested_protocol, actual_protocol, host, port, mode, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(domain, requested_protocol)
			 DO UPDATE SET actual_protocol=excluded.actual_protocol, host=excluded.host, port=excluded.port, mode=excluded.mode, updated_at=excluded.updated_at`,
			e.Domain, e.RequestedProtocol, e.ActualProtocol, e.Host, e.Port, e.Mode, time.Now().Format(time.RFC3339Nano),
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"created": true})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *WebApp) handleEndpointByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/endpoints/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	domain := parts[0]
	requestedProtocol := parts[1]

	switch r.Method {
	case http.MethodPut:
		var update EndpointCacheUpdate
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, err := a.db.Exec(
			`UPDATE endpoint_cache SET actual_protocol = ?, host = ?, port = ?, mode = ?, updated_at = ? WHERE domain = ? AND requested_protocol = ?`,
			update.ActualProtocol, update.Host, update.Port, update.Mode, time.Now().Format(time.RFC3339Nano), domain, requestedProtocol,
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"updated": true})

	case http.MethodDelete:
		_, err := a.db.Exec(`DELETE FROM endpoint_cache WHERE domain = ? AND requested_protocol = ?`, domain, requestedProtocol)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
