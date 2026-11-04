package daemon

import (
	"sync"
	"time"
)

// LogSubscriber is a channel that receives newly emitted log messages for a job.
type LogSubscriber chan *JobLog

// LogHub manages in-memory recent logs for active jobs and fans out lines to live subscribers.
type LogHub struct {
	mu          sync.RWMutex
	ringSize    int
	recentLogs  map[int64][]*JobLog
	subscribers map[int64]map[LogSubscriber]struct{}
}

// NewLogHub creates a new LogHub. ringSize is the number of recent lines retained per job.
func NewLogHub(ringSize int) *LogHub {
	if ringSize <= 0 {
		ringSize = 500
	}
	return &LogHub{
		ringSize:    ringSize,
		recentLogs:  make(map[int64][]*JobLog),
		subscribers: make(map[int64]map[LogSubscriber]struct{}),
	}
}

// Broadcast sends a log line to recent history and all active subscribers for jobID.
func (h *LogHub) Broadcast(jobID int64, stream, message string) *JobLog {
	entry := &JobLog{
		JobID:     jobID,
		Timestamp: time.Now().UTC(),
		Stream:    stream,
		Message:   message,
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// Append to ring buffer
	buf := h.recentLogs[jobID]
	if len(buf) >= h.ringSize {
		copy(buf, buf[1:])
		buf[len(buf)-1] = entry
	} else {
		buf = append(buf, entry)
	}
	h.recentLogs[jobID] = buf

	// Fan out to subscribers (non-blocking so slow clients don't stall execution)
	for sub := range h.subscribers[jobID] {
		select {
		case sub <- entry:
		default:
			// dropped if client buffer full
		}
	}
	return entry
}

// Subscribe registers a new subscriber for jobID and returns existing recent logs.
func (h *LogHub) Subscribe(jobID int64, bufferSize int) (LogSubscriber, []*JobLog) {
	if bufferSize <= 0 {
		bufferSize = 100
	}
	ch := make(LogSubscriber, bufferSize)

	h.mu.Lock()
	defer h.mu.Unlock()

	subs, ok := h.subscribers[jobID]
	if !ok {
		subs = make(map[LogSubscriber]struct{})
		h.subscribers[jobID] = subs
	}
	subs[ch] = struct{}{}

	// Copy current recent logs
	recent := make([]*JobLog, len(h.recentLogs[jobID]))
	copy(recent, h.recentLogs[jobID])

	return ch, recent
}

// Unsubscribe removes a subscriber channel.
func (h *LogHub) Unsubscribe(jobID int64, ch LogSubscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if subs, ok := h.subscribers[jobID]; ok {
		delete(subs, ch)
		if len(subs) == 0 {
			delete(h.subscribers, jobID)
		}
	}
	close(ch)
}

// Clear removes cached recent logs for jobID when no longer needed.
func (h *LogHub) Clear(jobID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.recentLogs, jobID)
}
