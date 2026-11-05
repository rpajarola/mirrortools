package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func TestServerBatchAndFilterEndpoints(t *testing.T) {
	srv, q := newTestServer(t)
	ctx := context.Background()

	// Enqueue jobs across sites
	s := q.store
	j1, err := s.EnqueueJob(ctx, &Job{Method: "http", Source: "https://siteA.com/docs/intro", DestDir: "/tmp/dest"})
	if err != nil {
		t.Fatalf("enqueue j1: %v", err)
	}
	_, err = s.EnqueueJob(ctx, &Job{Method: "http", Source: "https://siteA.com/docs/api", DestDir: "/tmp/dest"})
	if err != nil {
		t.Fatalf("enqueue j2: %v", err)
	}
	j3, err := s.EnqueueJob(ctx, &Job{Method: "http", Source: "https://siteB.com/images/logo.png", DestDir: "/tmp/dest"})
	if err != nil {
		t.Fatalf("enqueue j3: %v", err)
	}

	// Filter list by site
	req := httptest.NewRequest("GET", "/api/v1/jobs?site=siteA.com", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list jobs by site: %d", rec.Code)
	}
	var jobs []*Job
	json.Unmarshal(rec.Body.Bytes(), &jobs)
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs for siteA.com, got %d", len(jobs))
	}

	// Batch pause by prefix
	body, _ := json.Marshal(BatchRequest{
		Action: BatchPause,
		Prefix: "https://siteA.com/docs",
	})
	req = httptest.NewRequest("POST", "/api/v1/jobs/batch", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch pause: %d (%s)", rec.Code, rec.Body.String())
	}
	var res BatchResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Affected != 2 {
		t.Fatalf("expected 2 jobs paused, got %d", res.Affected)
	}

	// Verify j1 is paused, j3 is queued
	j1Updated, _ := s.GetJob(ctx, j1.ID)
	j3Updated, _ := s.GetJob(ctx, j3.ID)
	if j1Updated.State != StatePaused || j3Updated.State != StateQueued {
		t.Fatalf("expected j1 paused and j3 queued, got j1=%s j3=%s", j1Updated.State, j3Updated.State)
	}

	// Batch resume by site prefix
	body, _ = json.Marshal(BatchRequest{
		Action: BatchResume,
		Prefix: "siteA.com",
	})
	req = httptest.NewRequest("POST", "/api/v1/jobs/batch", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch resume: %d", rec.Code)
	}
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Affected != 2 {
		t.Fatalf("expected 2 jobs resumed, got %d", res.Affected)
	}

	// Single job pause and resume endpoints
	req = httptest.NewRequest("POST", fmt.Sprintf("/api/v1/jobs/%d/pause", j1.ID), nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause job %d: %d", j1.ID, rec.Code)
	}

	req = httptest.NewRequest("POST", fmt.Sprintf("/api/v1/jobs/%d/resume", j1.ID), nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume job %d: %d", j1.ID, rec.Code)
	}
}

func TestServerWithAuth(t *testing.T) {
	store := newTestStore(t)
	hub := NewLogHub(10)
	executor := &InProcessExecutor{}
	q := NewQueue(store, executor, hub, QueueConfig{Concurrency: 1})
	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			Auth: config.AuthConfig{
				Password: "secretpass",
			},
		},
	}
	srv := NewServer(q, cfg, "", nil)
	handler := srv.WithAuth()

	// 1. Unauthenticated request to / redirects to /auth/login
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/auth/login" {
		t.Fatalf("expected redirect to /auth/login, got code=%d loc=%s", rec.Code, rec.Header().Get("Location"))
	}

	// 2. Unauthenticated request to /api/ returns 401 Unauthorized
	req = httptest.NewRequest("GET", "/api/v1/queue/status", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated API, got %d", rec.Code)
	}

	// 3. Login page is public
	req = httptest.NewRequest("GET", "/auth/login", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login page should be 200, got %d", rec.Code)
	}

	// 4. Bad password fails
	form := "password=wrong"
	req = httptest.NewRequest("POST", "/auth/login", bytes.NewBufferString(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !bytes.Contains([]byte(rec.Header().Get("Location")), []byte("error=")) {
		t.Fatalf("expected redirect with error for bad password, got code=%d loc=%s", rec.Code, rec.Header().Get("Location"))
	}

	// 5. Correct password succeeds and sets session cookie
	form = "password=secretpass"
	req = httptest.NewRequest("POST", "/auth/login", bytes.NewBufferString(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("expected redirect to / on successful login, got code=%d loc=%s", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Fatalf("expected session cookie to be set")
	}

	// 6. Request with session cookie succeeds
	req = httptest.NewRequest("GET", "/api/v1/queue/status", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with session cookie, got %d", rec.Code)
	}

	// 7. Request with Bearer token matching password succeeds
	req = httptest.NewRequest("GET", "/api/v1/queue/status", nil)
	req.Header.Set("Authorization", "Bearer secretpass")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with Bearer token, got %d", rec.Code)
	}

	// 8. Logout invalidates session
	req = httptest.NewRequest("POST", "/auth/logout", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect on logout, got %d", rec.Code)
	}

	// 9. Old session cookie is now rejected
	req = httptest.NewRequest("GET", "/api/v1/queue/status", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d", rec.Code)
	}
}
