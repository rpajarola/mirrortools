package daemon

import (
	"encoding/json"
	"fmt"
	"net/url"
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
	Site             string            `json:"site"`
	Path             string            `json:"path"`
}

// ExtractRoot returns the site/root identifier for grouping URLs.
// For web URLs with a host: "scheme://host" (e.g. "https://example.com")
// For sourceforge project URLs: "https://sourceforge.net/projects/<project>"
// For other sources: method or base.
func ExtractRoot(source, method string) string {
	s := strings.TrimSpace(source)
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		host := strings.ToLower(u.Host)
		scheme := strings.ToLower(u.Scheme)
		if scheme == "" {
			scheme = "http"
		}
		if strings.Contains(host, "sourceforge.net") {
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) >= 2 && parts[0] == "projects" {
				return scheme + "://" + host + "/projects/" + parts[1]
			}
		}
		return scheme + "://" + host
	}
	if method != "" {
		return method
	}
	return "generic"
}

// ExtractPath returns the subdirectory path portion of a source.
func ExtractPath(source string) string {
	s := strings.TrimSpace(source)
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		p := u.Path
		if p == "" {
			p = "/"
		}
		if u.RawQuery != "" {
			p += "?" + u.RawQuery
		}
		return p
	}
	return s
}

// BatchAction defines an action applied to multiple jobs matching a prefix or site.
type BatchAction string

const (
	BatchPause        BatchAction = "pause"
	BatchResume       BatchAction = "resume"
	BatchQuarantine   BatchAction = "quarantine"
	BatchUnquarantine BatchAction = "unquarantine"
	BatchCancel       BatchAction = "cancel"
	BatchRetry        BatchAction = "retry"
	BatchDelete       BatchAction = "delete"
)

// BatchRequest specifies a batch action over jobs matching a prefix or list of IDs.
type BatchRequest struct {
	Action BatchAction `json:"action"`
	Prefix string      `json:"prefix,omitempty"` // URL or prefix (e.g. "https://example.com/docs/")
	IDs    []int64     `json:"ids,omitempty"`
	Reason string      `json:"reason,omitempty"` // optional for quarantine
}

// BatchResult reports the outcome of a batch action.
type BatchResult struct {
	Action   BatchAction `json:"action"`
	Target   string      `json:"target,omitempty"`
	Matched  int64       `json:"matched"`
	Affected int64       `json:"affected"`
	JobIDs   []int64     `json:"job_ids,omitempty"`
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
