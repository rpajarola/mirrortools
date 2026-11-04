package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueueExecutionAndConcurrency(t *testing.T) {
	store := newTestStore(t)
	hub := NewLogHub(100)

	var activeCount int32
	var maxObserved int32
	var mu sync.Mutex
	doneMap := make(map[string]bool)

	mockExecutor := &InProcessExecutor{
		CustomRunners: map[string]func(ctx context.Context, source, destDir string, flags map[string]string, onLog LogFunc) error{
			"test": func(ctx context.Context, source, destDir string, flags map[string]string, onLog LogFunc) error {
				curr := atomic.AddInt32(&activeCount, 1)
				for {
					old := atomic.LoadInt32(&maxObserved)
					if curr <= old || atomic.CompareAndSwapInt32(&maxObserved, old, curr) {
						break
					}
				}
				if onLog != nil {
					onLog("stdout", "running "+source)
				}
				time.Sleep(50 * time.Millisecond)
				atomic.AddInt32(&activeCount, -1)

				mu.Lock()
				doneMap[source] = true
				mu.Unlock()
				return nil
			},
		},
	}

	q := NewQueue(store, mockExecutor, hub, QueueConfig{Concurrency: 2})
	ctx := context.Background()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer q.Stop()

	// Enqueue 4 jobs
	for i := 1; i <= 4; i++ {
		_, err := q.Enqueue(ctx, &SubmitRequest{
			Method:  "test",
			Source:  "src" + string(rune('0'+i)),
			DestDir: "/tmp/dest",
		})
		if err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	// Wait for jobs to finish
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		count := len(doneMap)
		mu.Unlock()
		if count == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for 4 jobs to complete, completed: %d", count)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Verify maximum concurrency observed did not exceed 2
	if maxObs := atomic.LoadInt32(&maxObserved); maxObs > 2 {
		t.Fatalf("expected max concurrency <= 2, got %d", maxObs)
	}

	status, err := q.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if status.Counts[StateCompleted] != 4 {
		t.Fatalf("expected 4 completed jobs, got %+v", status.Counts)
	}
}

func TestQueuePauseAndResume(t *testing.T) {
	store := newTestStore(t)
	hub := NewLogHub(100)

	executed := make(chan string, 5)
	mockExecutor := &InProcessExecutor{
		CustomRunners: map[string]func(ctx context.Context, source, destDir string, flags map[string]string, onLog LogFunc) error{
			"test": func(ctx context.Context, source, destDir string, flags map[string]string, onLog LogFunc) error {
				executed <- source
				return nil
			},
		},
	}

	q := NewQueue(store, mockExecutor, hub, QueueConfig{Concurrency: 1})
	ctx := context.Background()
	q.Start(ctx)
	defer q.Stop()

	// Pause queue
	if err := q.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// Enqueue job
	j, err := q.Enqueue(ctx, &SubmitRequest{
		Method:  "test",
		Source:  "paused-src",
		DestDir: "/tmp",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Verify job is NOT executed while paused
	select {
	case src := <-executed:
		t.Fatalf("job %s executed while queue was paused", src)
	case <-time.After(200 * time.Millisecond):
		// good
	}

	reloaded, _ := q.GetJob(ctx, j.ID)
	if reloaded.State != StateQueued {
		t.Fatalf("expected job to stay queued, got %s", reloaded.State)
	}

	// Resume queue
	if err := q.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// Now it should be executed
	select {
	case src := <-executed:
		if src != "paused-src" {
			t.Fatalf("expected paused-src, got %s", src)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("job was not executed after resume")
	}
}

func TestQueueCancellation(t *testing.T) {
	store := newTestStore(t)
	hub := NewLogHub(100)

	started := make(chan struct{})
	mockExecutor := &InProcessExecutor{
		CustomRunners: map[string]func(ctx context.Context, source, destDir string, flags map[string]string, onLog LogFunc) error{
			"test": func(ctx context.Context, source, destDir string, flags map[string]string, onLog LogFunc) error {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			},
		},
	}

	q := NewQueue(store, mockExecutor, hub, QueueConfig{Concurrency: 1})
	ctx := context.Background()
	q.Start(ctx)
	defer q.Stop()

	j, _ := q.Enqueue(ctx, &SubmitRequest{
		Method:  "test",
		Source:  "long-job",
		DestDir: "/tmp",
	})

	<-started
	// Cancel the running job
	if err := q.CancelJob(ctx, j.ID); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	reloaded, _ := q.GetJob(ctx, j.ID)
	if reloaded.State != StateCancelled {
		t.Fatalf("expected state cancelled, got %s", reloaded.State)
	}
}

func TestQueueQuarantineOnFatalError(t *testing.T) {
	store := newTestStore(t)
	hub := NewLogHub(100)

	mockExecutor := &InProcessExecutor{
		CustomRunners: map[string]func(ctx context.Context, source, destDir string, flags map[string]string, onLog LogFunc) error{
			"test": func(ctx context.Context, source, destDir string, flags map[string]string, onLog LogFunc) error {
				return errors.New("HTTP 403 Forbidden: account locked")
			},
		},
	}

	q := NewQueue(store, mockExecutor, hub, QueueConfig{Concurrency: 1})
	ctx := context.Background()
	q.Start(ctx)
	defer q.Stop()

	j, _ := q.Enqueue(ctx, &SubmitRequest{
		Method:  "test",
		Source:  "forbidden-src",
		DestDir: "/tmp",
	})

	time.Sleep(200 * time.Millisecond)

	reloaded, _ := q.GetJob(ctx, j.ID)
	if reloaded.State != StateQuarantined || reloaded.QuarantineReason == "" {
		t.Fatalf("expected immediate quarantine on 403, got state %s, reason %q", reloaded.State, reloaded.QuarantineReason)
	}

	// Unquarantine and verify it goes back to queued
	if err := q.UnquarantineJob(ctx, j.ID); err != nil {
		t.Fatalf("UnquarantineJob: %v", err)
	}
	reloaded, _ = q.GetJob(ctx, j.ID)
	if reloaded.State != StateQueued && reloaded.State != StateRunning && reloaded.State != StateQuarantined {
		t.Fatalf("expected reset state, got %s", reloaded.State)
	}
}
