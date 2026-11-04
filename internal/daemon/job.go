package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// State describes the status of a mirror job in the queue.
type State string

const (
	StateQueued      State = "queued"
	StateRunning     State = "running"
	StatePaused      State = "paused"
	StateCompleted   State = "completed"
	StateFailed      State = "failed"
	StateQuarantined State = "quarantined"
	StateCancelled   State = "cancelled"
)

// Valid reports whether s is a recognized job state.
func (s State) Valid() bool {
	switch s {
	case StateQueued, StateRunning, StatePaused, StateCompleted, StateFailed, StateQuarantined, StateCancelled:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether s is a final state (no further automatic transitions).
func (s State) IsTerminal() bool {
	return s == StateCompleted || s == StateCancelled
}

// Job represents a single mirror request managed by the daemon queue.
type Job struct {
	ID               int64             `json:"id"`
	Method           string            `json:"method"`
	Source           string            `json:"source"`
	DestDir          string            `json:"dest_dir"`
	Flags            map[string]string `json:"flags,omitempty"`
	State            State             `json:"state"`
	Priority         int               `json:"priority"`
	CreatedAt        time.Time         `json:"created_at"`
	StartedAt        *time.Time        `json:"started_at,omitempty"`
	FinishedAt       *time.Time        `json:"finished_at,omitempty"`
	RetryCount       int               `json:"retry_count"`
	MaxRetries       int               `json:"max_retries"`
	LastError        string            `json:"last_error,omitempty"`
	QuarantineReason string            `json:"quarantine_reason,omitempty"`
}

// FlagsJSON returns the JSON representation of Flags for storing in the database.
func (j *Job) FlagsJSON() string {
	if len(j.Flags) == 0 {
		return "{}"
	}
	b, err := json.Marshal(j.Flags)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// SetFlagsJSON unmarshals raw JSON into j.Flags.
func (j *Job) SetFlagsJSON(raw string) {
	if strings.TrimSpace(raw) == "" || raw == "{}" {
		j.Flags = make(map[string]string)
		return
	}
	_ = json.Unmarshal([]byte(raw), &j.Flags)
}

// JobLog is one log message emitted by a job during execution.
type JobLog struct {
	ID        int64     `json:"id"`
	JobID     int64     `json:"job_id"`
	Timestamp time.Time `json:"timestamp"`
	Stream    string    `json:"stream"` // "stdout", "stderr", "info"
	Message   string    `json:"message"`
}

// QueueStatus represents a snapshot of the daemon queue.
type QueueStatus struct {
	Paused      bool          `json:"paused"`
	Concurrency int           `json:"concurrency"`
	MaxRetries  int           `json:"max_retries"`
	Counts      map[State]int `json:"counts"`
	ActiveJobs  []*Job        `json:"active_jobs,omitempty"`
	UptimeSec   int64         `json:"uptime_sec"`
}

// SubmitRequest is the payload sent to enqueue a new mirror job.
type SubmitRequest struct {
	Method   string            `json:"method,omitempty"` // optional; auto-detected if empty
	Source   string            `json:"source"`
	DestDir  string            `json:"dest_dir,omitempty"` // optional; uses config default if empty
	Flags    map[string]string `json:"flags,omitempty"`
	Priority int               `json:"priority,omitempty"`
}

// Validate checks that required fields are present in the request.
func (r *SubmitRequest) Validate() error {
	if strings.TrimSpace(r.Source) == "" {
		return fmt.Errorf("source cannot be empty")
	}
	return nil
}
