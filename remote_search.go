package main

import (
	"net/mail"
	"regexp"
	"sort"
	"strings"
	"time"
)

func (a *WebApp) searchAccounts(req SearchRequest) (*SearchResponse, error) {
	a.mu.RLock()
	selectedAccounts := selectAccounts(a.state.Accounts, req.AccountIDs, req.Emails)
	a.mu.RUnlock()

	fields := normalizeSearchFields(req.Fields)
	regex, err := compileOptionalRegex(req.Regex)
	if err != nil {
		return nil, err
	}

	cutoff := time.Time{}
	if req.RecentSeconds > 0 {
		cutoff = time.Now().Add(-time.Duration(req.RecentSeconds) * time.Second)
	}

	messages := make([]StoredMessage, 0)
	for _, stored := range selectedAccounts {
		runtimeAccount := Account{
			Email:        stored.Email,
			Password:     stored.Password,
			IMAPServer:   stored.Server,
			IMAPPort:     stored.Port,
			ClientID:     stored.ClientID,
			RefreshToken: stored.RefreshToken,
		}

		_, endpoint, err := a.resolveReceiveEndpoint(runtimeAccount, normalizeProtocol(stored.Protocol), a.fetchTimeout)
		if err != nil || endpoint == nil {
			continue
		}

		var summaries []*MessageSummary
		if endpoint.Protocol == "imap" {
			summaries, err = searchIMAPAccount(runtimeAccount, *endpoint, req, cutoff, fields, regex, a.fetchTimeout)
		} else {
			summaries, err = searchPOP3Account(runtimeAccount, *endpoint, req, cutoff, fields, regex, a.fetchTimeout)
		}
		if err != nil {
			continue
		}

		now := time.Now()
		for _, summary := range summaries {
			messages = append(messages, StoredMessage{
				ID:           messageSignature(stored.ID, summary),
				AccountID:    stored.ID,
				AccountEmail: stored.Email,
				RemoteID:     summary.ID,
				Folder:       summary.Folder,
				Protocol:     endpoint.Protocol,
				From:         summary.From,
				Subject:      summary.Subject,
				Date:         summary.Date,
				Body:         summary.Body,
				HTMLBody:     summary.rawHTMLBody,
				Preview:      previewText(summary.Body),
				SavedDir:     "",
				Attachments:  append([]string(nil), summary.Attachments...),
				FetchedAt:    now,
			})
		}
	}

	sort.Slice(messages, func(i, j int) bool {
		return compareRemoteMessage(messages[i], messages[j])
	})
	if len(messages) > req.Limit {
		messages = messages[:req.Limit]
	}
	for i := range messages {
		messages[i].Date = chinaDateText(messages[i].Date)
	}
	return &SearchResponse{Messages: messages}, nil
}

func searchIMAPAccount(account Account, endpoint Endpoint, req SearchRequest, cutoff time.Time, fields []string, regex *regexp.Regexp, timeout time.Duration) ([]*MessageSummary, error) {
	client, err := dialIMAP(endpoint, timeout)
	if err != nil {
		return nil, err
	}
	defer client.Logout()

	if account.ClientID != "" && account.RefreshToken != "" {
		token, err := requestOutlookAccessToken(account.ClientID, account.RefreshToken, outlookScope("imap"), timeout)
		if err != nil {
			return nil, err
		}
		if err := client.AuthXOAUTH2(account.Email, token); err != nil {
			return nil, err
		}
	} else if err := client.Login(account.Email, account.Password); err != nil {
		return nil, err
	}

	folders := req.Folders
	if len(folders) == 0 {
		folders = []string{defaultFolder}
	}
	criteria := buildIMAPSearchCriteria(req.Keyword, cutoff, fields)

	var results []*MessageSummary
	for _, folder := range folders {
		if _, err := client.Select(folder); err != nil {
			continue
		}
		ids, err := client.Search(criteria)
		if err != nil {
			continue
		}
		if len(ids) > req.ScanLimit {
			ids = ids[len(ids)-req.ScanLimit:]
		}
		for i := len(ids) - 1; i >= 0; i-- {
			rawMessage, err := client.FetchMessage(ids[i], true)
			if err != nil {
				continue
			}
			summary, err := saveMessage("", account.Email, ids[i], rawMessage)
			if err != nil {
				continue
			}
			summary.Folder = folder
			if !matchesRemoteSearch(summary, req.Keyword, cutoff, fields, regex) {
				continue
			}
			results = append(results, summary)
			if len(results) >= req.Limit {
				return results, nil
			}
		}
	}
	return results, nil
}

func searchPOP3Account(account Account, endpoint Endpoint, req SearchRequest, cutoff time.Time, fields []string, regex *regexp.Regexp, timeout time.Duration) ([]*MessageSummary, error) {
	client, err := dialPOP3(endpoint, timeout)
	if err != nil {
		return nil, err
	}
	defer client.Quit()

	if account.ClientID != "" && account.RefreshToken != "" {
		token, err := requestOutlookAccessToken(account.ClientID, account.RefreshToken, outlookScope("pop3"), timeout)
		if err != nil {
			return nil, err
		}
		if err := client.AuthXOAUTH2(account.Email, token); err != nil {
			return nil, err
		}
	} else if err := client.Login(account.Email, account.Password); err != nil {
		return nil, err
	}

	ids, err := client.List()
	if err != nil {
		return nil, err
	}
	if len(ids) > req.ScanLimit {
		ids = ids[len(ids)-req.ScanLimit:]
	}

	var results []*MessageSummary
	for i := len(ids) - 1; i >= 0; i-- {
		rawMessage, err := client.Retrieve(ids[i])
		if err != nil {
			continue
		}
		summary, err := saveMessage("", account.Email, ids[i], rawMessage)
		if err != nil {
			continue
		}
		if !matchesRemoteSearch(summary, req.Keyword, cutoff, fields, regex) {
			continue
		}
		results = append(results, summary)
		if len(results) >= req.Limit {
			break
		}
	}
	return results, nil
}

func buildIMAPSearchCriteria(keyword string, cutoff time.Time, fields []string) string {
	var parts []string
	if !cutoff.IsZero() {
		parts = append(parts, "SINCE "+cutoff.Format("2-Jan-2006"))
	}
	if keyword != "" {
		switch {
		case len(fields) == 1 && fields[0] == "subject":
			parts = append(parts, "SUBJECT "+imapString(keyword))
		case len(fields) == 1 && fields[0] == "from":
			parts = append(parts, "FROM "+imapString(keyword))
		case len(fields) == 1 && fields[0] == "body":
			parts = append(parts, "BODY "+imapString(keyword))
		default:
			parts = append(parts, "TEXT "+imapString(keyword))
		}
	}
	if len(parts) == 0 {
		return "ALL"
	}
	return strings.Join(parts, " ")
}

func matchesRemoteSearch(summary *MessageSummary, keyword string, cutoff time.Time, fields []string, regex *regexp.Regexp) bool {
	if !cutoff.IsZero() {
		messageTime := parseRemoteMailDate(summary.Date)
		if messageTime.IsZero() || messageTime.Before(cutoff) {
			return false
		}
	}

	values := collectSearchFields(summary, fields)
	if keyword != "" {
		lowKeyword := strings.ToLower(keyword)
		matched := false
		for _, value := range values {
			if strings.Contains(strings.ToLower(value), lowKeyword) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	if regex != nil {
		matched := false
		for _, value := range values {
			if regex.MatchString(value) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	return true
}

func collectSearchFields(summary *MessageSummary, fields []string) []string {
	var values []string
	for _, field := range fields {
		switch field {
		case "subject":
			values = append(values, summary.Subject)
		case "from":
			values = append(values, summary.From)
		case "body":
			values = append(values, summary.Body)
		}
	}
	if len(values) == 0 {
		return []string{summary.Subject, summary.From, summary.Body}
	}
	return values
}

func normalizeSearchFields(fields []string) []string {
	if len(fields) == 0 {
		return []string{"subject", "from", "body"}
	}
	seen := map[string]struct{}{}
	var normalized []string
	for _, field := range fields {
		field = strings.ToLower(strings.TrimSpace(field))
		switch field {
		case "all":
			return []string{"subject", "from", "body"}
		case "subject", "from", "body":
			if _, ok := seen[field]; ok {
				continue
			}
			seen[field] = struct{}{}
			normalized = append(normalized, field)
		}
	}
	if len(normalized) == 0 {
		return []string{"subject", "from", "body"}
	}
	return normalized
}

func compileOptionalRegex(pattern string) (*regexp.Regexp, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, nil
	}
	return regexp.Compile(pattern)
}

func parseRemoteMailDate(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := mail.ParseDate(value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func compareRemoteMessage(left, right StoredMessage) bool {
	leftDate := parseRemoteMailDate(left.Date)
	rightDate := parseRemoteMailDate(right.Date)
	if !leftDate.Equal(rightDate) {
		return leftDate.After(rightDate)
	}
	return left.FetchedAt.After(right.FetchedAt)
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
