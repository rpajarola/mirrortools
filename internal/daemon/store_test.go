package daemon

import (
	"context"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("OpenStore(:memory:) = %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStoreJobLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 1. Enqueue
	j1, err := s.EnqueueJob(ctx, &Job{
		Method:   "sourceforge",
		Source:   "https://sourceforge.net/projects/example/",
		DestDir:  "/tmp/dest",
		Flags:    map[string]string{"wait": "1s"},
		Priority: 10,
	})
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	if j1.ID == 0 || j1.State != StateQueued {
		t.Fatalf("expected ID > 0 and state queued, got %+v", j1)
	}

	j2, err := s.EnqueueJob(ctx, &Job{
		Method:   "archiveorg",
		Source:   "https://archive.org/details/item",
		DestDir:  "/tmp/dest",
		Priority: 20, // Higher priority
	})
	if err != nil {
		t.Fatalf("EnqueueJob j2: %v", err)
	}

	// 2. ClaimNextJob should pick j2 first because priority is higher
	claimed1, err := s.ClaimNextJob(ctx)
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}
	if claimed1.ID != j2.ID || claimed1.State != StateRunning || claimed1.StartedAt == nil {
		t.Fatalf("expected to claim j2 (%d), got %d (state: %s)", j2.ID, claimed1.ID, claimed1.State)
	}

	// 3. Claim j1
	claimed2, err := s.ClaimNextJob(ctx)
	if err != nil {
		t.Fatalf("ClaimNextJob 2: %v", err)
	}
	if claimed2.ID != j1.ID {
		t.Fatalf("expected to claim j1 (%d), got %d", j1.ID, claimed2.ID)
	}

	// 4. Queue should now be empty
	_, err = s.ClaimNextJob(ctx)
	if err != ErrQueueEmpty {
		t.Fatalf("expected ErrQueueEmpty, got %v", err)
	}

	// 5. Complete j2
	if err := s.FinishJob(ctx, j2.ID, StateCompleted, ""); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}
	reloadedJ2, _ := s.GetJob(ctx, j2.ID)
	if reloadedJ2.State != StateCompleted || reloadedJ2.FinishedAt == nil {
		t.Fatalf("expected j2 to be completed, got %+v", reloadedJ2)
	}

	// 6. Fail j1 with retry
	nextState, err := s.FailAndRetryJob(ctx, j1.ID, "temporary network timeout")
	if err != nil {
		t.Fatalf("FailAndRetryJob: %v", err)
	}
	if nextState != StateQueued {
		t.Fatalf("expected StateQueued on retry, got %s", nextState)
	}

	reloadedJ1, _ := s.GetJob(ctx, j1.ID)
	if reloadedJ1.RetryCount != 1 || reloadedJ1.LastError == "" {
		t.Fatalf("expected retry_count=1, got %+v", reloadedJ1)
	}

	// 7. Fail until quarantine (max_retries = 3)
	s.FailAndRetryJob(ctx, j1.ID, "err 2")
	finalState, err := s.FailAndRetryJob(ctx, j1.ID, "err 3")
	if err != nil {
		t.Fatalf("FailAndRetryJob: %v", err)
	}
	if finalState != StateQuarantined {
		t.Fatalf("expected quarantine after exceeding max_retries, got %s", finalState)
	}

	reloadedJ1, _ = s.GetJob(ctx, j1.ID)
	if reloadedJ1.State != StateQuarantined || reloadedJ1.QuarantineReason == "" {
		t.Fatalf("expected quarantined state with reason, got %+v", reloadedJ1)
	}

	// 8. Unquarantine j1
	if err := s.UnquarantineJob(ctx, j1.ID); err != nil {
		t.Fatalf("UnquarantineJob: %v", err)
	}
	reloadedJ1, _ = s.GetJob(ctx, j1.ID)
	if reloadedJ1.State != StateQueued || reloadedJ1.RetryCount != 0 {
		t.Fatalf("expected queued with reset retry count, got %+v", reloadedJ1)
	}
}

func TestStoreLogsAndRecovery(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	j, _ := s.EnqueueJob(ctx, &Job{
		Method:  "http",
		Source:  "https://example.com",
		DestDir: "/tmp/dest",
	})

	// Append logs
	s.AppendLog(ctx, j.ID, "stdout", "starting download")
	s.AppendLog(ctx, j.ID, "stderr", "warning: retrying request")

	logs, err := s.GetLogs(ctx, j.ID, 10, 0)
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if len(logs) != 2 || logs[0].Message != "starting download" || logs[1].Stream != "stderr" {
		t.Fatalf("unexpected logs: %+v", logs)
	}

	// Claim to running
	claimed, err := s.ClaimNextJob(ctx)
	if err != nil || claimed.ID != j.ID {
		t.Fatalf("ClaimNextJob: %v", err)
	}

	// Simulate crash recovery
	n, err := s.RecoverOrphanedJobs(ctx)
	if err != nil {
		t.Fatalf("RecoverOrphanedJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 recovered job, got %d", n)
	}

	recovered, _ := s.GetJob(ctx, j.ID)
	if recovered.State != StateQueued || recovered.RetryCount != 1 {
		t.Fatalf("expected job to be reset to queued with retry count 1, got %+v", recovered)
	}
}

func TestStoreQueueConfigAndCounts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	paused, concurrency, err := s.GetQueueConfig(ctx)
	if err != nil {
		t.Fatalf("GetQueueConfig: %v", err)
	}
	if paused || concurrency != 2 {
		t.Fatalf("expected default paused=false, concurrency=2, got paused=%v, concurrency=%d", paused, concurrency)
	}

	if err := s.SetQueuePaused(ctx, true); err != nil {
		t.Fatalf("SetQueuePaused: %v", err)
	}
	if err := s.SetQueueConcurrency(ctx, 5); err != nil {
		t.Fatalf("SetQueueConcurrency: %v", err)
	}

	paused, concurrency, _ = s.GetQueueConfig(ctx)
	if !paused || concurrency != 5 {
		t.Fatalf("expected paused=true, concurrency=5, got paused=%v, concurrency=%d", paused, concurrency)
	}

	// Check counts
	s.EnqueueJob(ctx, &Job{Method: "http", Source: "s1", DestDir: "/d"})
	s.EnqueueJob(ctx, &Job{Method: "http", Source: "s2", DestDir: "/d"})

	counts, err := s.GetCounts(ctx)
	if err != nil {
		t.Fatalf("GetCounts: %v", err)
	}
	if counts[StateQueued] != 2 {
		t.Fatalf("expected 2 queued, got %+v", counts)
	}
}
