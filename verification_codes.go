package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	verificationStatusPending = "pending"
	verificationStatusFound   = "found"

	mailboxSyncStatusSynced      = "synced"
	mailboxSyncStatusSyncing     = "syncing"
	mailboxSyncStatusNeverSynced = "never_synced"

	mailboxErrorAuthFailed = "mailbox_auth_failed"
	mailboxErrorTimeout    = "mailbox_timeout"
	mailboxErrorSyncFailed = "mailbox_sync_failed"
)

type VerificationCodeSearchRequest struct {
	MailboxID     string `json:"mailbox_id"`
	Service       string `json:"service"`
	ReceivedAfter string `json:"received_after"`
}

type VerificationCodeSearchResponse struct {
	Status       string     `json:"status"`
	Code         *string    `json:"code"`
	MessageID    *string    `json:"message_id"`
	ReceivedAt   *time.Time `json:"received_at"`
	LastSyncedAt *time.Time `json:"last_synced_at"`
	SyncStatus   string     `json:"sync_status"`
}

type verificationCodeAPIError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type verificationCodeErrorResponse struct {
	Error        verificationCodeAPIError `json:"error"`
	LastSyncedAt *time.Time               `json:"last_synced_at,omitempty"`
}

var redditCodePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:verification|confirmation|security|login)\s+code\s*(?:is)?\s*[:：]?\s*[*\s]*([0-9]{6})\b`),
	regexp.MustCompile(`(?i)\b([0-9]{6})\b\s+is\s+your\s+(?:reddit\s+)?(?:verification\s+)?code\b`),
	regexp.MustCompile(`(?i)\bcode\s*(?:is)?\s*[:：]?\s*[*\s]*([0-9]{6})\b`),
}

var genericCodePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:verification|confirmation|security|login|one[- ]time)\s+code\s*(?:is)?\s*[:：]?\s*[*\s]*([0-9]{4,8})\b`),
	regexp.MustCompile(`(?i)\b([0-9]{4,8})\b\s+is\s+your\s+(?:verification\s+)?code\b`),
	regexp.MustCompile(`验证码\s*(?:为|是|[:：])?\s*[*\s]*([0-9]{4,8})\b`),
}

func (a *WebApp) handleVerificationCodeSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeVerificationCodeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "请求方法必须是 POST", false, nil)
		return
	}

	var req VerificationCodeSearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeVerificationCodeError(w, http.StatusBadRequest, "invalid_request", "请求体必须是有效的 JSON", false, nil)
		return
	}
	req.MailboxID = strings.TrimSpace(req.MailboxID)
	req.Service = strings.ToLower(strings.TrimSpace(req.Service))
	if req.MailboxID == "" || req.Service == "" || strings.TrimSpace(req.ReceivedAfter) == "" {
		writeVerificationCodeError(w, http.StatusBadRequest, "invalid_request", "mailbox_id、service 和 received_after 均为必填项", false, nil)
		return
	}
	receivedAfter, err := time.Parse(time.RFC3339Nano, req.ReceivedAfter)
	if err != nil {
		writeVerificationCodeError(w, http.StatusBadRequest, "invalid_received_after", "received_after 必须是 RFC 3339 时间", false, nil)
		return
	}

	account, messages := a.verificationMailboxSnapshot(req.MailboxID)
	if account == nil {
		writeVerificationCodeError(w, http.StatusNotFound, "mailbox_not_found", "邮箱不存在", false, nil)
		return
	}
	lastSyncedAt := utcTimePointer(account.LastFetchedAt)
	syncing := a.isMailboxSyncing(account.ID)
	if !syncing && (account.LastError != "" || account.LastErrorCode != "") {
		code := account.LastErrorCode
		if code == "" {
			code = classifyMailboxError(errors.New(account.LastError))
		}
		switch code {
		case mailboxErrorAuthFailed:
			writeVerificationCodeError(w, http.StatusUnauthorized, code, "邮箱登录凭据已失效", false, lastSyncedAt)
		case mailboxErrorTimeout:
			writeVerificationCodeError(w, http.StatusGatewayTimeout, code, "邮件供应商响应超时", true, lastSyncedAt)
		default:
			writeVerificationCodeError(w, http.StatusBadGateway, mailboxErrorSyncFailed, "邮箱同步失败："+account.LastError, true, lastSyncedAt)
		}
		return
	}

	syncStatus := mailboxSyncStatusSynced
	if syncing {
		syncStatus = mailboxSyncStatusSyncing
	} else if account.LastFetchedAt == nil {
		syncStatus = mailboxSyncStatusNeverSynced
	}
	response := VerificationCodeSearchResponse{
		Status:       verificationStatusPending,
		LastSyncedAt: lastSyncedAt,
		SyncStatus:   syncStatus,
	}
	if message, receivedAt, code := findVerificationCode(messages, req.Service, receivedAfter); message != nil {
		response.Status = verificationStatusFound
		response.Code = &code
		response.MessageID = &message.ID
		response.ReceivedAt = utcTimePointer(&receivedAt)
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *WebApp) verificationMailboxSnapshot(mailboxID string) (*StoredAccount, []StoredMessage) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var account *StoredAccount
	for i := range a.state.Accounts {
		if a.state.Accounts[i].ID == mailboxID {
			copyAccount := a.state.Accounts[i]
			account = &copyAccount
			break
		}
	}
	if account == nil {
		for i := range a.state.Accounts {
			if strings.EqualFold(a.state.Accounts[i].Email, mailboxID) {
				copyAccount := a.state.Accounts[i]
				account = &copyAccount
				break
			}
		}
	}
	if account == nil {
		return nil, nil
	}

	messages := make([]StoredMessage, 0)
	for _, message := range a.state.Messages {
		if message.AccountID == account.ID {
			messages = append(messages, message)
		}
	}
	return account, messages
}

func findVerificationCode(messages []StoredMessage, service string, receivedAfter time.Time) (*StoredMessage, time.Time, string) {
	var bestMessage *StoredMessage
	var bestReceivedAt time.Time
	var bestCode string
	for i := range messages {
		message := &messages[i]
		receivedAt := parseRemoteMailDate(message.Date)
		if receivedAt.IsZero() || receivedAt.Before(receivedAfter) || !messageMatchesService(*message, service) {
			continue
		}
		code := extractVerificationCode(*message, service)
		if code == "" {
			continue
		}
		if bestMessage == nil || receivedAt.After(bestReceivedAt) || (receivedAt.Equal(bestReceivedAt) && message.FetchedAt.After(bestMessage.FetchedAt)) {
			copyMessage := *message
			bestMessage = &copyMessage
			bestReceivedAt = receivedAt
			bestCode = code
		}
	}
	return bestMessage, bestReceivedAt, bestCode
}

func messageMatchesService(message StoredMessage, service string) bool {
	haystack := strings.ToLower(strings.Join([]string{message.From, message.Subject, message.Body}, "\n"))
	return strings.Contains(haystack, service)
}

func extractVerificationCode(message StoredMessage, service string) string {
	patterns := genericCodePatterns
	if service == "reddit" {
		patterns = redditCodePatterns
	}
	text := message.Subject + "\n" + message.Body
	for _, pattern := range patterns {
		matches := pattern.FindStringSubmatch(text)
		if len(matches) == 2 {
			return matches[1]
		}
	}
	return ""
}

func (a *WebApp) startMailboxSync(accountID string) {
	a.fetchJobsMu.Lock()
	defer a.fetchJobsMu.Unlock()
	a.mailboxSyncs[accountID]++
}

func (a *WebApp) finishMailboxSync(accountID string) {
	a.fetchJobsMu.Lock()
	defer a.fetchJobsMu.Unlock()
	if a.mailboxSyncs[accountID] <= 1 {
		delete(a.mailboxSyncs, accountID)
		return
	}
	a.mailboxSyncs[accountID]--
}

func (a *WebApp) isMailboxSyncing(accountID string) bool {
	a.fetchJobsMu.RLock()
	defer a.fetchJobsMu.RUnlock()
	return a.mailboxSyncs[accountID] > 0
}

func classifyMailboxError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return mailboxErrorTimeout
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return mailboxErrorTimeout
	}

	text := strings.ToLower(err.Error())
	authMarkers := []string{
		"unsafe login", "login error or password error", "unable to log on",
		"authentication failed", "invalid credentials", "bad credentials",
		"invalid_grant", "not authorized", "密码或授权码错误", "风险登录", "登录失败",
	}
	for _, marker := range authMarkers {
		if strings.Contains(text, marker) {
			return mailboxErrorAuthFailed
		}
	}
	if strings.Contains(text, "timeout") || strings.Contains(text, "deadline exceeded") || strings.Contains(text, "连接超时") {
		return mailboxErrorTimeout
	}
	return mailboxErrorSyncFailed
}

func writeVerificationCodeError(w http.ResponseWriter, httpStatus int, code, message string, retryable bool, lastSyncedAt *time.Time) {
	writeJSON(w, httpStatus, verificationCodeErrorResponse{
		Error: verificationCodeAPIError{
			Code:      code,
			Message:   message,
			Retryable: retryable,
		},
		LastSyncedAt: lastSyncedAt,
	})
}

func utcTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
