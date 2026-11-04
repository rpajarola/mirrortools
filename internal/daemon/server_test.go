package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rpajarola/mirrortools/internal/config"
	_ "github.com/rpajarola/mirrortools/sourceforge"
)

func newTestServer(t *testing.T) (*Server, *Queue) {
	t.Helper()
	store := newTestStore(t)
	hub := NewLogHub(100)
	executor := &InProcessExecutor{
		CustomRunners: map[string]func(ctx context.Context, source, destDir string, flags map[string]string, onLog LogFunc) error{
			"sourceforge": func(ctx context.Context, source, destDir string, flags map[string]string, onLog LogFunc) error {
				if onLog != nil {
					onLog("stdout", "downloading "+source)
				}
				return nil
			},
		},
	}
	q := NewQueue(store, executor, hub, QueueConfig{Concurrency: 2})
	cfg := &config.Config{
		DestDir: "/tmp/default_dest",
		Methods: map[string]*config.MethodConfig{},
	}
	srv := NewServer(q, cfg, "", nil)
	return srv, q
}

func TestServerEndpoints(t *testing.T) {
	srv, q := newTestServer(t)
	ctx := context.Background()
	q.Start(ctx)
	defer q.Stop()

	// 1. Get queue status
	req := httptest.NewRequest("GET", "/api/v1/queue/status", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("queue status: expected 200, got %d", rec.Code)
	}

	// 2. Submit a job (testing auto-detection of sourceforge)
	body, _ := json.Marshal(map[string]any{
		"source": "https://sourceforge.net/projects/fsd-amoeba/",
	})
	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("submit job: expected 201, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	var createdJob Job
	if err := json.Unmarshal(rec.Body.Bytes(), &createdJob); err != nil {
		t.Fatalf("decode created job: %v", err)
	}
	if createdJob.Method != "sourceforge" {
		t.Fatalf("expected auto-detected method sourceforge, got %s", createdJob.Method)
	}
	if createdJob.DestDir == "" {
		t.Fatalf("expected destdir to be auto-filled from default, got empty")
	}

	// 3. List jobs
	req = httptest.NewRequest("GET", "/api/v1/jobs", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list jobs: expected 200, got %d", rec.Code)
	}
	var jobs []*Job
	json.Unmarshal(rec.Body.Bytes(), &jobs)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}

	// 4. Pause queue
	req = httptest.NewRequest("POST", "/api/v1/queue/pause", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause queue: expected 200, got %d", rec.Code)
	}

	// 5. Resume queue
	req = httptest.NewRequest("POST", "/api/v1/queue/resume", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume queue: expected 200, got %d", rec.Code)
	}

	// 6. Concurrency update
	body, _ = json.Marshal(map[string]int{"concurrency": 5})
	req = httptest.NewRequest("POST", "/api/v1/queue/concurrency", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("concurrency: expected 200, got %d", rec.Code)
	}
}
