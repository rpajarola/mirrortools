package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// QueueConfig holds initial settings for the queue.
type QueueConfig struct {
	Concurrency int
	MaxRetries  int
}

// Queue coordinates job persistence, concurrency limits, execution, and state transitions.
type Queue struct {
	store    *Store
	executor Executor
	logHub   *LogHub

	mu             sync.RWMutex
	paused         bool
	concurrency    int
	maxRetries     int
	startTime      time.Time
	runningCancels map[int64]context.CancelFunc
	activeJobs     map[int64]*Job

	notifyCh chan struct{}
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewQueue initializes a queue with the given store, executor, and logHub.
func NewQueue(store *Store, executor Executor, logHub *LogHub, cfg QueueConfig) *Queue {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if logHub == nil {
		logHub = NewLogHub(500)
	}

	return &Queue{
		store:          store,
		executor:       executor,
		logHub:         logHub,
		concurrency:    cfg.Concurrency,
		maxRetries:     cfg.MaxRetries,
		runningCancels: make(map[int64]context.CancelFunc),
		activeJobs:     make(map[int64]*Job),
		notifyCh:       make(chan struct{}, 1),
		stopCh:         make(chan struct{}),
	}
}

// Start begins processing the queue.
func (q *Queue) Start(ctx context.Context) error {
	q.mu.Lock()
	q.startTime = time.Now().UTC()

	// Recover any jobs orphaned from a prior crash or termination
	if n, err := q.store.RecoverOrphanedJobs(ctx); err == nil && n > 0 {
		log.Printf("mirrord: recovered %d orphaned jobs from previous run", n)
	}

	// Load persistent settings if available
	paused, conc, err := q.store.GetQueueConfig(ctx)
	if err == nil {
		q.paused = paused
		if conc > 0 {
			q.concurrency = conc
		}
	}
	q.mu.Unlock()

	q.wg.Add(1)
	go q.dispatchLoop()

	log.Printf("mirrord: queue started (concurrency: %d, paused: %v)", q.concurrency, q.paused)
	return nil
}

// Stop gracefully shuts down the queue, aborting active jobs and waiting for workers.
func (q *Queue) Stop() {
	close(q.stopCh)

	q.mu.Lock()
	for id, cancel := range q.runningCancels {
		cancel()
		delete(q.runningCancels, id)
	}
	q.mu.Unlock()

	q.wg.Wait()
	log.Printf("mirrord: queue stopped")
}

// notify wakes up the dispatcher loop to check for runnable work.
func (q *Queue) notify() {
	select {
	case q.notifyCh <- struct{}{}:
	default:
	}
}

// dispatchLoop continuously assigns queued jobs to worker goroutines up to concurrency limit.
func (q *Queue) dispatchLoop() {
	defer q.wg.Done()

	for {
		select {
		case <-q.stopCh:
			return
		default:
		}

		q.mu.RLock()
		paused := q.paused
		activeCount := len(q.activeJobs)
		concurrency := q.concurrency
		q.mu.RUnlock()

		if paused || activeCount >= concurrency {
			select {
			case <-q.notifyCh:
				continue
			case <-time.After(500 * time.Millisecond):
				continue
			case <-q.stopCh:
				return
			}
		}

		// Try to claim the next job
		job, err := q.store.ClaimNextJob(context.Background())
		if err != nil {
			if errors.Is(err, ErrQueueEmpty) {
				// Nothing to run right now; wait for notification or timeout
				select {
				case <-q.notifyCh:
				case <-time.After(1 * time.Second):
				case <-q.stopCh:
					return
				}
				continue
			}
			log.Printf("mirrord: error claiming next job: %v", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// Spawn a worker for the claimed job
		jobCtx, jobCancel := context.WithCancel(context.Background())

		q.mu.Lock()
		q.activeJobs[job.ID] = job
		q.runningCancels[job.ID] = jobCancel
		q.mu.Unlock()

		q.wg.Add(1)
		go q.runWorker(jobCtx, job)
	}
}

// isImmediateQuarantineError reports whether an error warrants immediate quarantine.
func isImmediateQuarantineError(err error) (bool, string) {
	if err == nil {
		return false, ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "password") || strings.Contains(msg, "auth failed") || strings.Contains(msg, "invalid credentials"):
		return true, "Authentication or password failed"
	case strings.Contains(msg, "403 forbidden") || strings.Contains(msg, "account locked") || strings.Contains(msg, "access restricted"):
		return true, "Access denied or account restricted (HTTP 403)"
	case strings.Contains(msg, "cookie rotation") || strings.Contains(msg, "session no longer valid"):
		return true, "Session/cookies expired or invalid"
	}
	return false, ""
}

// runWorker executes a single mirror job with full log capture and state updates.
func (q *Queue) runWorker(ctx context.Context, job *Job) {
	defer q.wg.Done()
	defer func() {
		q.mu.Lock()
		delete(q.activeJobs, job.ID)
		delete(q.runningCancels, job.ID)
		q.mu.Unlock()
		q.notify()
	}()

	log.Printf("mirrord: [job #%d] starting %s %s", job.ID, job.Method, job.Source)

	onLog := func(stream, line string) {
		q.logHub.Broadcast(job.ID, stream, line)
		// Save log line to persistent store
		_ = q.store.AppendLog(context.Background(), job.ID, stream, line)
	}

	runErr := q.executor.Run(ctx, job, onLog)

	// Determine final outcome
	if ctx.Err() != nil {
		// Context was cancelled
		log.Printf("mirrord: [job #%d] cancelled", job.ID)
		_ = q.store.FinishJob(context.Background(), job.ID, StateCancelled, "cancelled by user")
		return
	}

	if runErr == nil {
		log.Printf("mirrord: [job #%d] finished successfully", job.ID)
		_ = q.store.FinishJob(context.Background(), job.ID, StateCompleted, "")
		return
	}

	// Execution failed
	log.Printf("mirrord: [job #%d] failed: %v", job.ID, runErr)
	if shouldQuarantine, reason := isImmediateQuarantineError(runErr); shouldQuarantine {
		log.Printf("mirrord: [job #%d] quarantined immediately: %s", job.ID, reason)
		_ = q.store.QuarantineJob(context.Background(), job.ID, fmt.Sprintf("%s: %v", reason, runErr))
		return
	}

	// Standard retry or quarantine after max retries
	nextState, err := q.store.FailAndRetryJob(context.Background(), job.ID, runErr.Error())
	if err != nil {
		log.Printf("mirrord: [job #%d] error updating failure state: %v", job.ID, err)
		return
	}
	if nextState == StateQuarantined {
		log.Printf("mirrord: [job #%d] reached max retries, moved to quarantined", job.ID)
	} else {
		log.Printf("mirrord: [job #%d] scheduled for retry", job.ID)
	}
}

// Enqueue adds a new job to the queue.
func (q *Queue) Enqueue(ctx context.Context, req *SubmitRequest) (*Job, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	job := &Job{
		Method:     req.Method,
		Source:     req.Source,
		DestDir:    req.DestDir,
		Flags:      req.Flags,
		Priority:   req.Priority,
		MaxRetries: q.maxRetries,
	}

	job, err := q.store.EnqueueJob(ctx, job)
	if err != nil {
		return nil, err
	}

	q.notify()
	return job, nil
}

// Pause pauses the queue (running jobs continue, but no new jobs start).
func (q *Queue) Pause(ctx context.Context) error {
	q.mu.Lock()
	q.paused = true
	q.mu.Unlock()

	return q.store.SetQueuePaused(ctx, true)
}

// Resume unpauses the queue.
func (q *Queue) Resume(ctx context.Context) error {
	q.mu.Lock()
	q.paused = false
	q.mu.Unlock()

	err := q.store.SetQueuePaused(ctx, false)
	q.notify()
	return err
}

// SetConcurrency updates the maximum concurrent workers.
func (q *Queue) SetConcurrency(ctx context.Context, n int) error {
	if n < 1 {
		return fmt.Errorf("concurrency must be at least 1")
	}

	q.mu.Lock()
	q.concurrency = n
	q.mu.Unlock()

	err := q.store.SetQueueConcurrency(ctx, n)
	q.notify()
	return err
}

// CancelJob cancels a job if running, or marks it cancelled if queued/paused.
func (q *Queue) CancelJob(ctx context.Context, id int64) error {
	q.mu.Lock()
	if cancel, ok := q.runningCancels[id]; ok {
		cancel()
		q.mu.Unlock()
		return nil
	}
	q.mu.Unlock()

	return q.store.CancelJob(ctx, id)
}

// PauseJob pauses a queued job.
func (q *Queue) PauseJob(ctx context.Context, id int64) error {
	return q.store.PauseJob(ctx, id)
}

// ResumeJob unpauses an individual job.
func (q *Queue) ResumeJob(ctx context.Context, id int64) error {
	err := q.store.ResumeJob(ctx, id)
	q.notify()
	return err
}

// RetryJob resets a failed or quarantined job back to queued.
func (q *Queue) RetryJob(ctx context.Context, id int64) (*Job, error) {
	job, err := q.store.RetryJob(ctx, id)
	if err == nil {
		q.notify()
	}
	return job, err
}

// QuarantineJob manually quarantines a job.
func (q *Queue) QuarantineJob(ctx context.Context, id int64, reason string) error {
	// If currently running, cancel it first
	q.mu.Lock()
	if cancel, ok := q.runningCancels[id]; ok {
		cancel()
	}
	q.mu.Unlock()

	return q.store.QuarantineJob(ctx, id, reason)
}

// UnquarantineJob moves a quarantined job back to queued.
func (q *Queue) UnquarantineJob(ctx context.Context, id int64) error {
	err := q.store.UnquarantineJob(ctx, id)
	if err == nil {
		q.notify()
	}
	return err
}

// GetStatus returns the current queue metrics and active jobs.
func (q *Queue) GetStatus(ctx context.Context) (*QueueStatus, error) {
	q.mu.RLock()
	paused := q.paused
	concurrency := q.concurrency
	uptime := int64(time.Since(q.startTime).Seconds())
	activeList := make([]*Job, 0, len(q.activeJobs))
	for _, j := range q.activeJobs {
		activeList = append(activeList, j)
	}
	q.mu.RUnlock()

	counts, err := q.store.GetCounts(ctx)
	if err != nil {
		return nil, err
	}

	return &QueueStatus{
		Paused:      paused,
		Concurrency: concurrency,
		MaxRetries:  q.maxRetries,
		Counts:      counts,
		ActiveJobs:  activeList,
		UptimeSec:   uptime,
	}, nil
}

// GetJob returns a job by ID.
func (q *Queue) GetJob(ctx context.Context, id int64) (*Job, error) {
	return q.store.GetJob(ctx, id)
}

// ListJobs queries jobs.
func (q *Queue) ListJobs(ctx context.Context, filter JobFilter) ([]*Job, error) {
	return q.store.ListJobs(ctx, filter)
}

// GetLogs returns stored logs for a job.
func (q *Queue) GetLogs(ctx context.Context, id int64, limit, offset int) ([]*JobLog, error) {
	return q.store.GetLogs(ctx, id, limit, offset)
}

// LogHub returns the log hub instance.
func (q *Queue) LogHub() *LogHub {
	return q.logHub
}
