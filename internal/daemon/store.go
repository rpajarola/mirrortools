package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrJobNotFound = errors.New("job not found")
	ErrQueueEmpty  = errors.New("no jobs available in queue")
)

// JobFilter allows querying jobs with specific criteria.
type JobFilter struct {
	States []State
	Method string
	Limit  int
	Offset int
}

// Store handles persistence of jobs, logs, and queue settings in SQLite.
type Store struct {
	db *sql.DB
	mu sync.Mutex // guards transactions where single-conn serialization is desired
}

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
PRAGMA synchronous=NORMAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS jobs (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    method            TEXT NOT NULL,
    source            TEXT NOT NULL,
    dest_dir          TEXT NOT NULL,
    flags_json        TEXT NOT NULL DEFAULT '{}',
    state             TEXT NOT NULL,
    priority          INTEGER NOT NULL DEFAULT 0,
    created_at        DATETIME NOT NULL,
    started_at        DATETIME,
    finished_at       DATETIME,
    retry_count       INTEGER NOT NULL DEFAULT 0,
    max_retries       INTEGER NOT NULL DEFAULT 3,
    last_error        TEXT,
    quarantine_reason TEXT
);

CREATE INDEX IF NOT EXISTS idx_jobs_state_prio ON jobs(state, priority DESC, created_at ASC);

CREATE TABLE IF NOT EXISTS job_logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id      INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    timestamp   DATETIME NOT NULL,
    stream      TEXT NOT NULL,
    message     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_job_logs_job_id ON job_logs(job_id, id ASC);

CREATE TABLE IF NOT EXISTS queue_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// OpenStore opens or creates the SQLite database at dbPath, applying migrations.
func OpenStore(dbPath string) (*Store, error) {
	if dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return nil, fmt.Errorf("creating directory for db %s: %w", dbPath, err)
		}
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening db %s: %w", dbPath, err)
	}

	// modernc.org/sqlite works best with bounded connections to avoid lock contention
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initializing db schema: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying SQLite database.
func (s *Store) Close() error {
	return s.db.Close()
}

// RecoverOrphanedJobs resets jobs that were marked running when the daemon
// stopped/crashed into either queued (if retries remaining) or failed.
func (s *Store) RecoverOrphanedJobs(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET state = 'queued',
		    retry_count = retry_count + 1,
		    last_error = 'daemon restarted while job was running',
		    finished_at = ?
		WHERE state = 'running' AND retry_count < max_retries;
	`, now)
	if err != nil {
		return 0, err
	}
	recovered, _ := res.RowsAffected()

	// Any that exceeded max retries become quarantined
	_, err = s.db.ExecContext(ctx, `
		UPDATE jobs
		SET state = 'quarantined',
		    quarantine_reason = 'daemon restarted and max retries exceeded',
		    finished_at = ?
		WHERE state = 'running';
	`, now)
	if err != nil {
		return int(recovered), err
	}

	return int(recovered), nil
}

// EnqueueJob inserts a new job in Queued state.
func (s *Store) EnqueueJob(ctx context.Context, job *Job) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now().UTC()
	}
	if job.State == "" {
		job.State = StateQueued
	}
	if job.MaxRetries <= 0 {
		job.MaxRetries = 3
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (method, source, dest_dir, flags_json, state, priority, created_at, retry_count, max_retries)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);
	`, job.Method, job.Source, job.DestDir, job.FlagsJSON(), job.State, job.Priority, job.CreatedAt, job.RetryCount, job.MaxRetries)
	if err != nil {
		return nil, fmt.Errorf("inserting job: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	job.ID = id
	return job, nil
}

// GetJob loads a job by ID.
func (s *Store) GetJob(ctx context.Context, id int64) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	row := s.db.QueryRowContext(ctx, `
		SELECT id, method, source, dest_dir, flags_json, state, priority,
		       created_at, started_at, finished_at, retry_count, max_retries,
		       COALESCE(last_error, ''), COALESCE(quarantine_reason, '')
		FROM jobs WHERE id = ?;
	`, id)

	return scanJob(row)
}

// ClaimNextJob finds the highest priority oldest queued job and transitions it to running.
// Returns ErrQueueEmpty if no job is available.
func (s *Store) ClaimNextJob(ctx context.Context) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `
		SELECT id, method, source, dest_dir, flags_json, state, priority,
		       created_at, started_at, finished_at, retry_count, max_retries,
		       COALESCE(last_error, ''), COALESCE(quarantine_reason, '')
		FROM jobs
		WHERE state = 'queued'
		ORDER BY priority DESC, created_at ASC
		LIMIT 1;
	`)

	job, err := scanJob(row)
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			return nil, ErrQueueEmpty
		}
		return nil, err
	}

	now := time.Now().UTC()
	job.State = StateRunning
	job.StartedAt = &now

	_, err = tx.ExecContext(ctx, `
		UPDATE jobs SET state = ?, started_at = ? WHERE id = ?;
	`, job.State, now, job.ID)
	if err != nil {
		return nil, fmt.Errorf("claiming job: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return job, nil
}

// FinishJob records completion (success or final failure) of a job.
func (s *Store) FinishJob(ctx context.Context, id int64, state State, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET state = ?, finished_at = ?, last_error = ?
		WHERE id = ?;
	`, state, now, errMsg, id)
	return err
}

// FailAndRetryJob increments the retry count and moves the job back to queued,
// or quarantines it if max_retries has been exceeded.
func (s *Store) FailAndRetryJob(ctx context.Context, id int64, errMsg string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var retries, maxRetries int
	err = tx.QueryRowContext(ctx, `SELECT retry_count, max_retries FROM jobs WHERE id = ?;`, id).Scan(&retries, &maxRetries)
	if err != nil {
		return "", err
	}

	retries++
	now := time.Now().UTC()
	var nextState State
	var qReason string

	if retries >= maxRetries {
		nextState = StateQuarantined
		qReason = fmt.Sprintf("exceeded maximum retries (%d): %s", maxRetries, errMsg)
		_, err = tx.ExecContext(ctx, `
			UPDATE jobs
			SET state = ?, retry_count = ?, last_error = ?, quarantine_reason = ?, finished_at = ?
			WHERE id = ?;
		`, nextState, retries, errMsg, qReason, now, id)
	} else {
		nextState = StateQueued
		_, err = tx.ExecContext(ctx, `
			UPDATE jobs
			SET state = ?, retry_count = ?, last_error = ?
			WHERE id = ?;
		`, nextState, retries, errMsg, id)
	}
	if err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return nextState, nil
}

// QuarantineJob explicitly moves a job to quarantined state with a reason.
func (s *Store) QuarantineJob(ctx context.Context, id int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET state = 'quarantined', quarantine_reason = ?, finished_at = ?
		WHERE id = ?;
	`, reason, now, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrJobNotFound
	}
	return nil
}

// UnquarantineJob resets a quarantined job to queued state.
func (s *Store) UnquarantineJob(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET state = 'queued', quarantine_reason = '', last_error = '', retry_count = 0
		WHERE id = ? AND state = 'quarantined';
	`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("job %d is not quarantined", id)
	}
	return nil
}

// RetryJob resets any failed or completed job back into queued state.
func (s *Store) RetryJob(ctx context.Context, id int64) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET state = 'queued', last_error = '', quarantine_reason = '', retry_count = 0, started_at = NULL, finished_at = NULL
		WHERE id = ?;
	`, id)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, ErrJobNotFound
	}

	row := s.db.QueryRowContext(ctx, `
		SELECT id, method, source, dest_dir, flags_json, state, priority,
		       created_at, started_at, finished_at, retry_count, max_retries,
		       COALESCE(last_error, ''), COALESCE(quarantine_reason, '')
		FROM jobs WHERE id = ?;
	`, id)
	return scanJob(row)
}

// PauseJob sets an individual queued job to paused.
func (s *Store) PauseJob(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET state = 'paused' WHERE id = ? AND state = 'queued';
	`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("job %d cannot be paused (only queued jobs can be paused)", id)
	}
	return nil
}

// ResumeJob unpauses an individual job back to queued.
func (s *Store) ResumeJob(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET state = 'queued' WHERE id = ? AND state = 'paused';
	`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("job %d is not paused", id)
	}
	return nil
}

// CancelJob cancels a queued, running, or paused job.
func (s *Store) CancelJob(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET state = 'cancelled', finished_at = ?
		WHERE id = ? AND state IN ('queued', 'running', 'paused');
	`, now, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("job %d cannot be cancelled", id)
	}
	return nil
}

// DeleteJob permanently removes a terminal job (completed, failed, cancelled).
func (s *Store) DeleteJob(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `
		DELETE FROM jobs WHERE id = ? AND state IN ('completed', 'failed', 'cancelled', 'quarantined');
	`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("job %d cannot be deleted (must be completed, failed, cancelled, or quarantined)", id)
	}
	return nil
}

// ListJobs returns jobs matching the filter.
func (s *Store) ListJobs(ctx context.Context, filter JobFilter) ([]*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var where []string
	var args []any

	if len(filter.States) > 0 {
		placeholders := make([]string, len(filter.States))
		for i, st := range filter.States {
			placeholders[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, fmt.Sprintf("state IN (%s)", strings.Join(placeholders, ",")))
	}
	if filter.Method != "" {
		where = append(where, "method = ?")
		args = append(args, filter.Method)
	}

	query := `
		SELECT id, method, source, dest_dir, flags_json, state, priority,
		       created_at, started_at, finished_at, retry_count, max_retries,
		       COALESCE(last_error, ''), COALESCE(quarantine_reason, '')
		FROM jobs
	`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id DESC"

	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", filter.Limit)
		if filter.Offset > 0 {
			query += fmt.Sprintf(" OFFSET %d", filter.Offset)
		}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		job, err := scanJobRow(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// GetCounts returns the number of jobs in each state.
func (s *Store) GetCounts(ctx context.Context) (map[State]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	counts := map[State]int{
		StateQueued:      0,
		StateRunning:     0,
		StatePaused:      0,
		StateCompleted:   0,
		StateFailed:      0,
		StateQuarantined: 0,
		StateCancelled:   0,
	}

	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM jobs GROUP BY state;`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var st string
		var c int
		if err := rows.Scan(&st, &c); err == nil {
			counts[State(st)] = c
		}
	}
	return counts, rows.Err()
}

// AppendLog writes a log line for a job.
func (s *Store) AppendLog(ctx context.Context, jobID int64, stream, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO job_logs (job_id, timestamp, stream, message)
		VALUES (?, ?, ?, ?);
	`, jobID, now, stream, message)
	return err
}

// GetLogs fetches logs for a job in chronological order.
func (s *Store) GetLogs(ctx context.Context, jobID int64, limit, offset int) ([]*JobLog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `SELECT id, job_id, timestamp, stream, message FROM job_logs WHERE job_id = ? ORDER BY id ASC`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
		if offset > 0 {
			query += fmt.Sprintf(" OFFSET %d", offset)
		}
	}

	rows, err := s.db.QueryContext(ctx, query, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []*JobLog
	for rows.Next() {
		var l JobLog
		if err := rows.Scan(&l.ID, &l.JobID, &l.Timestamp, &l.Stream, &l.Message); err != nil {
			return nil, err
		}
		logs = append(logs, &l)
	}
	return logs, rows.Err()
}

// GetQueueConfig reads paused and concurrency settings from the database.
func (s *Store) GetQueueConfig(ctx context.Context) (paused bool, concurrency int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	concurrency = 2 // default
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM queue_state;`)
	if err != nil {
		return false, concurrency, err
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err == nil {
			switch k {
			case "paused":
				paused = (v == "1" || v == "true")
			case "concurrency":
				fmt.Sscanf(v, "%d", &concurrency)
			}
		}
	}
	if concurrency < 1 {
		concurrency = 1
	}
	return paused, concurrency, nil
}

// SetQueuePaused sets the global pause state.
func (s *Store) SetQueuePaused(ctx context.Context, paused bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	v := "0"
	if paused {
		v = "1"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO queue_state (key, value) VALUES ('paused', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;
	`, v)
	return err
}

// SetQueueConcurrency sets the global concurrency limit.
func (s *Store) SetQueueConcurrency(ctx context.Context, concurrency int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if concurrency < 1 {
		concurrency = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO queue_state (key, value) VALUES ('concurrency', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;
	`, fmt.Sprint(concurrency))
	return err
}

// rowScanner abstracts *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row *sql.Row) (*Job, error) {
	var j Job
	var flagsJSON string
	var startedAt, finishedAt sql.NullTime

	err := row.Scan(
		&j.ID, &j.Method, &j.Source, &j.DestDir, &flagsJSON, &j.State, &j.Priority,
		&j.CreatedAt, &startedAt, &finishedAt, &j.RetryCount, &j.MaxRetries,
		&j.LastError, &j.QuarantineReason,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	if startedAt.Valid {
		j.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		j.FinishedAt = &finishedAt.Time
	}
	j.SetFlagsJSON(flagsJSON)
	return &j, nil
}

func scanJobRow(rows *sql.Rows) (*Job, error) {
	var j Job
	var flagsJSON string
	var startedAt, finishedAt sql.NullTime

	err := rows.Scan(
		&j.ID, &j.Method, &j.Source, &j.DestDir, &flagsJSON, &j.State, &j.Priority,
		&j.CreatedAt, &startedAt, &finishedAt, &j.RetryCount, &j.MaxRetries,
		&j.LastError, &j.QuarantineReason,
	)
	if err != nil {
		return nil, err
	}
	if startedAt.Valid {
		j.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		j.FinishedAt = &finishedAt.Time
	}
	j.SetFlagsJSON(flagsJSON)
	return &j, nil
}
