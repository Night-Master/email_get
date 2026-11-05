package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type CachedEndpoint struct {
	Domain            string
	RequestedProtocol string
	Endpoint          Endpoint
	UpdatedAt         time.Time
}

func (a *WebApp) initDB() error {
	dbPath := filepath.Join(a.dataDir, "app.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	a.db = db

	schema := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL,
			password TEXT NOT NULL,
			server TEXT,
			port INTEGER,
			client_id TEXT,
			refresh_token TEXT,
			protocol TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			last_fetched_at TEXT,
			last_error TEXT,
			last_error_code TEXT,
			last_protocol TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			account_email TEXT NOT NULL,
			remote_id INTEGER NOT NULL,
			folder TEXT,
			protocol TEXT NOT NULL,
			sender TEXT,
			subject TEXT,
			date_text TEXT,
			body TEXT,
			html_body TEXT,
			preview TEXT,
			saved_dir TEXT,
			attachments_json TEXT,
			fetched_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS endpoint_cache (
			domain TEXT NOT NULL,
			requested_protocol TEXT NOT NULL,
			actual_protocol TEXT NOT NULL,
			host TEXT NOT NULL,
			port INTEGER NOT NULL,
			mode TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(domain, requested_protocol)
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}

	// 老库迁移:messages 表补 folder 列(已存在时忽略错误)
	if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN folder TEXT`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	// 老库迁移:messages 表补 html_body 列(已存在时忽略错误)
	if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN html_body TEXT`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	if _, err := db.Exec(`ALTER TABLE accounts ADD COLUMN last_error_code TEXT`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}

	return nil
}

func (a *WebApp) migrateFromJSONIfNeeded() error {
	if a.statePath == "" {
		return nil
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	data, err := os.ReadFile(a.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var state AppState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	state.Messages = dedupeMessages(state.Messages)
	a.state = state
	return a.saveStateToDB()
}

func (a *WebApp) loadStateFromDB() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	state := AppState{}

	rows, err := a.db.Query(`SELECT id, email, password, server, port, client_id, refresh_token, protocol, created_at, updated_at, last_fetched_at, last_error, last_error_code, last_protocol FROM accounts ORDER BY created_at DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var account StoredAccount
		var server, clientID, refreshToken, lastFetchedAt, lastError, lastErrorCode, lastProtocol sql.NullString
		var port sql.NullInt64
		var createdAt, updatedAt string
		if err := rows.Scan(
			&account.ID,
			&account.Email,
			&account.Password,
			&server,
			&port,
			&clientID,
			&refreshToken,
			&account.Protocol,
			&createdAt,
			&updatedAt,
			&lastFetchedAt,
			&lastError,
			&lastErrorCode,
			&lastProtocol,
		); err != nil {
			return err
		}
		account.Server = nullableString(server)
		account.Port = int(nullableInt64(port))
		account.ClientID = nullableString(clientID)
		account.RefreshToken = nullableString(refreshToken)
		account.CreatedAt = parseTimeOrZero(createdAt)
		account.UpdatedAt = parseTimeOrZero(updatedAt)
		if lastFetchedAt.Valid {
			t := parseTimeOrZero(lastFetchedAt.String)
			account.LastFetchedAt = &t
		}
		account.LastError = nullableString(lastError)
		account.LastErrorCode = nullableString(lastErrorCode)
		account.LastProtocol = nullableString(lastProtocol)
		state.Accounts = append(state.Accounts, account)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	messageRows, err := a.db.Query(`SELECT id, account_id, account_email, remote_id, folder, protocol, sender, subject, date_text, body, html_body, preview, saved_dir, attachments_json, fetched_at FROM messages ORDER BY fetched_at DESC`)
	if err != nil {
		return err
	}
	defer messageRows.Close()

	for messageRows.Next() {
		var message StoredMessage
		var attachmentsJSON, savedDir, folder, htmlBody sql.NullString
		var fetchedAt string
		if err := messageRows.Scan(
			&message.ID,
			&message.AccountID,
			&message.AccountEmail,
			&message.RemoteID,
			&folder,
			&message.Protocol,
			&message.From,
			&message.Subject,
			&message.Date,
			&message.Body,
			&htmlBody,
			&message.Preview,
			&savedDir,
			&attachmentsJSON,
			&fetchedAt,
		); err != nil {
			return err
		}
		message.Folder = nullableString(folder)
		message.HTMLBody = nullableString(htmlBody)
		message.SavedDir = nullableString(savedDir)
		message.FetchedAt = parseTimeOrZero(fetchedAt)
		if attachmentsJSON.Valid && attachmentsJSON.String != "" {
			_ = json.Unmarshal([]byte(attachmentsJSON.String), &message.Attachments)
		}
		state.Messages = append(state.Messages, message)
	}
	if err := messageRows.Err(); err != nil {
		return err
	}

	state.Messages = dedupeMessages(state.Messages)
	a.state = state
	return nil
}

func (a *WebApp) saveStateToDB() error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM accounts`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM messages`); err != nil {
		return err
	}

	for _, account := range a.state.Accounts {
		var lastFetchedAt any
		if account.LastFetchedAt != nil {
			lastFetchedAt = account.LastFetchedAt.Format(time.RFC3339Nano)
		}
		if _, err := tx.Exec(
			`INSERT INTO accounts (id, email, password, server, port, client_id, refresh_token, protocol, created_at, updated_at, last_fetched_at, last_error, last_error_code, last_protocol)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			account.ID,
			account.Email,
			account.Password,
			emptyToNil(account.Server),
			zeroToNil(account.Port),
			emptyToNil(account.ClientID),
			emptyToNil(account.RefreshToken),
			account.Protocol,
			account.CreatedAt.Format(time.RFC3339Nano),
			account.UpdatedAt.Format(time.RFC3339Nano),
			lastFetchedAt,
			emptyToNil(account.LastError),
			emptyToNil(account.LastErrorCode),
			emptyToNil(account.LastProtocol),
		); err != nil {
			return err
		}
	}

	for _, message := range a.state.Messages {
		attachmentsJSON, err := json.Marshal(message.Attachments)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO messages (id, account_id, account_email, remote_id, folder, protocol, sender, subject, date_text, body, html_body, preview, saved_dir, attachments_json, fetched_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			message.ID,
			message.AccountID,
			message.AccountEmail,
			message.RemoteID,
			emptyToNil(message.Folder),
			message.Protocol,
			message.From,
			message.Subject,
			message.Date,
			message.Body,
			emptyToNil(message.HTMLBody),
			message.Preview,
			emptyToNil(message.SavedDir),
			string(attachmentsJSON),
			message.FetchedAt.Format(time.RFC3339Nano),
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (a *WebApp) loadCachedEndpoint(domain, requestedProtocol string) (*Endpoint, error) {
	row := a.db.QueryRow(`SELECT actual_protocol, host, port, mode FROM endpoint_cache WHERE domain = ? AND requested_protocol = ?`, domain, requestedProtocol)
	var endpoint Endpoint
	if err := row.Scan(&endpoint.Protocol, &endpoint.Host, &endpoint.Port, &endpoint.Mode); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	endpoint.Source = "cache"
	return &endpoint, nil
}

func (a *WebApp) saveCachedEndpoint(domain, requestedProtocol string, endpoint Endpoint) error {
	_, err := a.db.Exec(
		`INSERT INTO endpoint_cache (domain, requested_protocol, actual_protocol, host, port, mode, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(domain, requested_protocol)
		 DO UPDATE SET actual_protocol=excluded.actual_protocol, host=excluded.host, port=excluded.port, mode=excluded.mode, updated_at=excluded.updated_at`,
		domain,
		requestedProtocol,
		endpoint.Protocol,
		endpoint.Host,
		endpoint.Port,
		endpoint.Mode,
		time.Now().Format(time.RFC3339Nano),
	)
	return err
}

func nullableString(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

func nullableInt64(value sql.NullInt64) int64 {
	if value.Valid {
		return value.Int64
	}
	return 0
}

func parseTimeOrZero(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return t
}

func emptyToNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func zeroToNil(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func (a *WebApp) debugCacheSummary() string {
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM endpoint_cache`).Scan(&count); err != nil {
		return fmt.Sprintf("cache_error:%v", err)
	}
	return fmt.Sprintf("cache:%d", count)
}

func (a *WebApp) loadProxySettings() error {
	enabled, rawList, err := a.getProxySettings()
	if err != nil {
		return err
	}
	return proxyState.apply(enabled, rawList)
}

func (a *WebApp) getProxySettings() (bool, string, error) {
	rows, err := a.db.Query(`SELECT key, value FROM settings WHERE key IN ('proxy_enabled', 'proxy_list')`)
	if err != nil {
		return false, "", err
	}
	defer rows.Close()

	enabled := false
	rawList := ""
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return false, "", err
		}
		switch key {
		case "proxy_enabled":
			enabled = value == "1"
		case "proxy_list":
			rawList = value
		}
	}
	if err := rows.Err(); err != nil {
		return false, "", err
	}
	return enabled, rawList, nil
}

func (a *WebApp) getProxySettingsView() (*ProxySettingsView, error) {
	rows, err := a.db.Query(`SELECT key, value, updated_at FROM settings WHERE key IN ('proxy_enabled', 'proxy_list')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	enabled := false
	rawList := ""
	updatedAt := ""
	for rows.Next() {
		var key, value, ts string
		if err := rows.Scan(&key, &value, &ts); err != nil {
			return nil, err
		}
		switch key {
		case "proxy_enabled":
			enabled = value == "1"
			if ts > updatedAt {
				updatedAt = ts
			}
		case "proxy_list":
			rawList = value
			if ts > updatedAt {
				updatedAt = ts
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return buildProxySettingsView(enabled, rawList, updatedAt)
}

func (a *WebApp) saveProxySettings(enabled bool, rawList string) (*ProxySettingsView, error) {
	view, err := buildProxySettingsView(enabled, rawList, time.Now().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	if err := proxyState.apply(view.Enabled, view.RawList); err != nil {
		return nil, err
	}

	tx, err := a.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().Format(time.RFC3339Nano)
	if _, err := tx.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		"proxy_enabled", boolToSettingValue(view.Enabled), now,
	); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		"proxy_list", view.RawList, now,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	view.UpdatedAt = now
	return view, nil
}

func boolToSettingValue(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
