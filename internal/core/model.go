// Package core implements durable task state and the reminder scheduler.
package core

import "time"

const (
	PlatformRain         = "rain"
	PlatformZhidaoShared = "zhidao_shared"
	PlatformZhidaoCampus = "zhidao_campus"
)

type Task struct {
	ID             string     `json:"id"`
	ExternalID     string     `json:"external_id,omitempty"`
	Platform       string     `json:"platform"`
	Course         string     `json:"course"`
	Title          string     `json:"title"`
	Kind           string     `json:"kind"`
	Status         string     `json:"status"`
	StartAt        *time.Time `json:"start_at"`
	EndAt          *time.Time `json:"end_at"`
	Progress       *float64   `json:"progress"`
	SourceURL      string     `json:"source_url,omitempty"`
	RawTime        string     `json:"raw_time,omitempty"`
	SourceTimezone string     `json:"source_timezone,omitempty"`
	Historical     bool       `json:"historical"`
	LastSyncedAt   time.Time  `json:"last_synced_at"`
}

type Settings struct {
	Timezone         string `json:"timezone"`
	QuietEnabled     bool   `json:"quiet_enabled"`
	QuietStart       string `json:"quiet_start"`
	QuietEnd         string `json:"quiet_end"`
	DeadlineMinutes  []int  `json:"deadline_minutes"`
	ExamStartMinutes []int  `json:"exam_start_minutes"`
	VideoMinutes     []int  `json:"video_minutes"`
	// InClass watches live Rain Classroom lessons and notifies as soon as a new
	// answerable activity is pushed. PollSeconds is the fast-loop interval.
	InClassEnabled     bool `json:"in_class_enabled"`
	InClassPollSeconds int  `json:"in_class_poll_seconds"`
}

// DefaultInClassPollSeconds keeps a live class responsive without hammering the
// platform. A class quiz is usually answered within minutes, so 20 seconds is
// the compromise between latency and request volume on a small device.
const DefaultInClassPollSeconds = 20

func DefaultSettings() Settings {
	return Settings{Timezone: "Asia/Shanghai", QuietEnabled: true, QuietStart: "23:00", QuietEnd: "08:00", DeadlineMinutes: []int{1440, 180, 30}, ExamStartMinutes: []int{1440, 60}, VideoMinutes: []int{4320, 1440, 180}, InClassEnabled: true, InClassPollSeconds: DefaultInClassPollSeconds}
}

type Account struct {
	Platform            string     `json:"platform"`
	LoginState          string     `json:"login_state"`
	SyncState           string     `json:"sync_state"`
	LastSuccessAt       *time.Time `json:"last_success_at"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastError           string     `json:"last_error,omitempty"`
	NeedsRevalidation   bool       `json:"needs_revalidation"`
	Incident            int        `json:"incident"`
	FaultNotified       bool       `json:"fault_notified"`
}

type Event struct {
	ID            string     `json:"id"`
	TaskID        string     `json:"task_id,omitempty"`
	Platform      string     `json:"platform"`
	Kind          string     `json:"kind"`
	Title         string     `json:"title"`
	Body          string     `json:"body"`
	DueAt         time.Time  `json:"due_at"`
	CreatedAt     time.Time  `json:"created_at"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	LastError     string     `json:"last_error,omitempty"`
	SentAt        *time.Time `json:"sent_at"`
	AnchorAt      *time.Time `json:"anchor_at,omitempty"`
	MinutesBefore int        `json:"minutes_before,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
}

type State struct {
	Version  int       `json:"version"`
	Tasks    []Task    `json:"tasks"`
	Events   []Event   `json:"events"`
	Accounts []Account `json:"accounts"`
	Settings Settings  `json:"settings"`
	// InClassSeen records already-handled in-class activities so a restart or a
	// repeated poll cannot notify the same quiz twice. Values are detection times
	// and are pruned after a week.
	InClassSeen map[string]time.Time `json:"in_class_seen,omitempty"`
}
