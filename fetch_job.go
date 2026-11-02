package main

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

const (
	fetchJobStatusQueued    = "queued"
	fetchJobStatusRunning   = "running"
	fetchJobStatusCompleted = "completed"
	fetchJobStatusFailed    = "failed"
	maxFetchJobHistory      = 100
)

type FetchJobAccount struct {
	AccountID string `json:"accountId"`
	Email     string `json:"email"`
}

type FetchJob struct {
	ID                string               `json:"id"`
	Status            string               `json:"status"`
	TotalAccounts     int                  `json:"totalAccounts"`
	CompletedAccounts int                  `json:"completedAccounts"`
	SuccessCount      int                  `json:"successCount"`
	FailureCount      int                  `json:"failureCount"`
	CurrentAccountID  string               `json:"currentAccountId,omitempty"`
	CurrentEmail      string               `json:"currentEmail,omitempty"`
	Results           []FetchAccountResult `json:"results,omitempty"`
	Targets           []FetchJobAccount    `json:"targets,omitempty"`
	Skipped           []FetchJobAccount    `json:"skipped,omitempty"`
	Message           string               `json:"message,omitempty"`
	Error             string               `json:"error,omitempty"`
	CreatedAt         time.Time            `json:"createdAt"`
	StartedAt         *time.Time           `json:"startedAt,omitempty"`
	FinishedAt        *time.Time           `json:"finishedAt,omitempty"`

	mu sync.RWMutex `json:"-"`
}

func newFetchJob(selectedAccounts []StoredAccount) *FetchJob {
	now := time.Now()
	targets := make([]FetchJobAccount, 0, len(selectedAccounts))
	for _, account := range selectedAccounts {
		targets = append(targets, FetchJobAccount{AccountID: account.ID, Email: account.Email})
	}
	return &FetchJob{
		ID:            randomID(),
		Status:        fetchJobStatusQueued,
		TotalAccounts: len(selectedAccounts),
		Results:       make([]FetchAccountResult, 0, len(selectedAccounts)),
		Targets:       targets,
		CreatedAt:     now,
		Message:       "等待开始",
	}
}

func (j *FetchJob) clone() *FetchJob {
	if j == nil {
		return nil
	}
	j.mu.RLock()
	defer j.mu.RUnlock()

	copyJob := &FetchJob{
		ID:                j.ID,
		Status:            j.Status,
		TotalAccounts:     j.TotalAccounts,
		CompletedAccounts: j.CompletedAccounts,
		SuccessCount:      j.SuccessCount,
		FailureCount:      j.FailureCount,
		CurrentAccountID:  j.CurrentAccountID,
		CurrentEmail:      j.CurrentEmail,
		Results:           append([]FetchAccountResult(nil), j.Results...),
		Targets:           append([]FetchJobAccount(nil), j.Targets...),
		Skipped:           append([]FetchJobAccount(nil), j.Skipped...),
		Message:           j.Message,
		Error:             j.Error,
		CreatedAt:         j.CreatedAt,
	}
	if j.StartedAt != nil {
		startedAt := *j.StartedAt
		copyJob.StartedAt = &startedAt
	}
	if j.FinishedAt != nil {
		finishedAt := *j.FinishedAt
		copyJob.FinishedAt = &finishedAt
	}
	return copyJob
}

func (j *FetchJob) isActive() bool {
	if j == nil {
		return false
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.Status == fetchJobStatusQueued || j.Status == fetchJobStatusRunning
}

func (j *FetchJob) markRunning() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.StartedAt == nil {
		now := time.Now()
		j.StartedAt = &now
	}
	j.Status = fetchJobStatusRunning
	j.Message = "后台收件中"
	if j.TotalAccounts == 0 {
		j.CurrentAccountID = ""
		j.CurrentEmail = ""
	}
}

func (j *FetchJob) startAccount(account StoredAccount) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.StartedAt == nil {
		now := time.Now()
		j.StartedAt = &now
	}
	j.Status = fetchJobStatusRunning
	j.CurrentAccountID = account.ID
	j.CurrentEmail = account.Email
	j.Message = fmt.Sprintf("正在收取 %s", account.Email)
}

func (j *FetchJob) appendResult(result FetchAccountResult) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Results = append(j.Results, result)
	j.CompletedAccounts++
	if result.Error != "" {
		j.FailureCount++
	} else {
		j.SuccessCount++
	}
	if j.CompletedAccounts >= j.TotalAccounts {
		j.CurrentAccountID = ""
		j.CurrentEmail = ""
		j.Message = fmt.Sprintf("收件完成，成功 %d，失败 %d", j.SuccessCount, j.FailureCount)
		return
	}
	remaining := j.TotalAccounts - j.CompletedAccounts
	j.Message = fmt.Sprintf("已完成 %d/%d，剩余 %d", j.CompletedAccounts, j.TotalAccounts, remaining)
}

func (j *FetchJob) addSkipped(account StoredAccount) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Skipped = append(j.Skipped, FetchJobAccount{AccountID: account.ID, Email: account.Email})
}

func (j *FetchJob) finish() {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	if j.StartedAt == nil {
		j.StartedAt = &now
	}
	j.FinishedAt = &now
	j.Status = fetchJobStatusCompleted
	j.CurrentAccountID = ""
	j.CurrentEmail = ""
	if j.Message == "" {
		j.Message = fmt.Sprintf("收件完成，成功 %d，失败 %d", j.SuccessCount, j.FailureCount)
	}
}

func (j *FetchJob) fail(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	if j.StartedAt == nil {
		j.StartedAt = &now
	}
	j.FinishedAt = &now
	j.Status = fetchJobStatusFailed
	j.CurrentAccountID = ""
	j.CurrentEmail = ""
	if err != nil {
		j.Error = err.Error()
		j.Message = err.Error()
	} else {
		j.Message = "收件任务失败"
	}
}

func (a *WebApp) startFetchJob(req FetchRequest) (*FetchJobResponse, int, error) {
	selectedAccounts := a.selectedAccounts(req)
	if len(selectedAccounts) == 0 {
		return nil, 0, fmt.Errorf("请先选择账号")
	}

	job := newFetchJob(selectedAccounts)
	startedAccounts := make([]StoredAccount, 0, len(selectedAccounts))
	skipped := make([]FetchJobAccount, 0)

	a.fetchJobsMu.Lock()
	for _, account := range selectedAccounts {
		if _, busy := a.activeFetchOwners[account.ID]; busy {
			skipped = append(skipped, FetchJobAccount{AccountID: account.ID, Email: account.Email})
			job.addSkipped(account)
			continue
		}
		a.activeFetchOwners[account.ID] = job.ID
		startedAccounts = append(startedAccounts, account)
	}
	job.TotalAccounts = len(startedAccounts)
	job.Targets = make([]FetchJobAccount, 0, len(startedAccounts))
	for _, account := range startedAccounts {
		job.Targets = append(job.Targets, FetchJobAccount{AccountID: account.ID, Email: account.Email})
	}
	a.fetchJobs[job.ID] = job
	a.fetchJobOrder = append(a.fetchJobOrder, job.ID)
	a.trimFetchJobsLocked(maxFetchJobHistory)
	a.fetchJobsMu.Unlock()

	if len(startedAccounts) == 0 {
		job.fail(fmt.Errorf("所选账号都在收件中"))
		return &FetchJobResponse{
			Job:     job.clone(),
			Started: false,
			Message: "所选账号都在收件中",
			Skipped: skipped,
		}, httpStatusOK, nil
	}

	go a.runFetchJob(job, req, startedAccounts)

	message := "后台收件已开始"
	if len(skipped) > 0 {
		message = fmt.Sprintf("后台收件已开始，跳过 %d 个正在收件的账号", len(skipped))
	}
	return &FetchJobResponse{
		Job:     job.clone(),
		Started: true,
		Message: message,
		Skipped: skipped,
	}, httpStatusAccepted, nil
}

func (a *WebApp) runFetchJob(job *FetchJob, req FetchRequest, selectedAccounts []StoredAccount) {
	defer func() {
		if recovered := recover(); recovered != nil {
			job.fail(fmt.Errorf("收件任务异常: %v", recovered))
		}
		a.releaseFetchOwners(job.ID, selectedAccounts)
	}()

	job.markRunning()
	a.fetchSelectedAccounts(req, selectedAccounts, job.startAccount, func(_ StoredAccount, result FetchAccountResult) {
		job.appendResult(result)
	})
	job.finish()
}

func (a *WebApp) latestActiveFetchJob() *FetchJob {
	a.fetchJobsMu.RLock()
	defer a.fetchJobsMu.RUnlock()
	active := a.latestActiveFetchJobLocked()
	if active == nil {
		return nil
	}
	return active.clone()
}

func (a *WebApp) activeFetchJobs() []*FetchJob {
	a.fetchJobsMu.RLock()
	defer a.fetchJobsMu.RUnlock()
	jobs := make([]*FetchJob, 0)
	for _, id := range a.fetchJobOrder {
		job := a.fetchJobs[id]
		if job != nil && job.isActive() {
			jobs = append(jobs, job.clone())
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].CreatedAt.After(jobs[j].CreatedAt)
	})
	return jobs
}

func (a *WebApp) latestActiveFetchJobLocked() *FetchJob {
	for i := len(a.fetchJobOrder) - 1; i >= 0; i-- {
		job := a.fetchJobs[a.fetchJobOrder[i]]
		if job != nil && job.isActive() {
			return job
		}
	}
	return nil
}

func (a *WebApp) getFetchJob(id string) *FetchJob {
	a.fetchJobsMu.RLock()
	defer a.fetchJobsMu.RUnlock()
	job := a.fetchJobs[id]
	if job == nil {
		return nil
	}
	return job.clone()
}

func (a *WebApp) releaseFetchOwners(jobID string, accounts []StoredAccount) {
	a.fetchJobsMu.Lock()
	defer a.fetchJobsMu.Unlock()
	for _, account := range accounts {
		if owner, ok := a.activeFetchOwners[account.ID]; ok && owner == jobID {
			delete(a.activeFetchOwners, account.ID)
		}
	}
}

func (a *WebApp) trimFetchJobsLocked(limit int) {
	if limit <= 0 || len(a.fetchJobOrder) <= limit {
		return
	}
	removeCount := len(a.fetchJobOrder) - limit
	for i := 0; i < removeCount; i++ {
		id := a.fetchJobOrder[0]
		a.fetchJobOrder = a.fetchJobOrder[1:]
		delete(a.fetchJobs, id)
	}
}

const (
	httpStatusOK       = 200
	httpStatusAccepted = 202
)
