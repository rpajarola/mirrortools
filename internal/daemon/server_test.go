package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func TestServerSiteTransfersAndIgnoresEndpoints(t *testing.T) {
	srv, q := newTestServer(t)
	ctx := context.Background()

	destDir := t.TempDir()

	// 1. Enqueue a job for site
	_, err := q.store.EnqueueJob(ctx, &Job{
		Method:  "http",
		Source:  "https://mysite.com/pub/",
		DestDir: destDir,
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}

	// 2. Query transfers before .httpmirror.db exists (fallback to jobs)
	req := httptest.NewRequest("GET", "/api/v1/sites/transfers?site=mysite.com", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get transfers without db: %d", rec.Code)
	}
	var resp SiteTransfersResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Site != "mysite.com" && resp.Site != "https://mysite.com" {
		t.Fatalf("expected site in response, got %s", resp.Site)
	}
	if resp.Counts.Future != 1 {
		t.Fatalf("expected 1 future job in fallback, got %+v", resp.Counts)
	}

	// 3. Create a real .httpmirror.db in destDir
	rawDB, err := sql.Open("sqlite", filepath.Join(destDir, ".httpmirror.db"))
	if err != nil {
		t.Fatalf("open .httpmirror.db: %v", err)
	}
	defer rawDB.Close()

	_, err = rawDB.Exec(`
		CREATE TABLE urls (
			id INTEGER PRIMARY KEY,
			url TEXT NOT NULL UNIQUE,
			requisite INTEGER NOT NULL,
			state TEXT NOT NULL DEFAULT 'pending',
			status INTEGER,
			error TEXT,
			etag TEXT,
			last_modified TEXT
		);
		CREATE TABLE ignored_patterns (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pattern TEXT NOT NULL UNIQUE,
			created_at DATETIME NOT NULL
		);
		INSERT INTO urls (url, requisite, state, status) VALUES ('https://mysite.com/pub/index.html', 0, 'done', 200);
		INSERT INTO urls (url, requisite, state, status, error) VALUES ('http://deadhost.com/icon.ico', 1, 'error', 0, 'no such host');
		INSERT INTO urls (url, requisite, state) VALUES ('https://mysite.com/pub/next.html', 0, 'pending');
	`)
	if err != nil {
		t.Fatalf("populate .httpmirror.db: %v", err)
	}

	// 4. Query transfers with .httpmirror.db
	req = httptest.NewRequest("GET", "/api/v1/sites/transfers?site=mysite.com", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get transfers with db: %d", rec.Code)
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.HasCrawlerDB {
		t.Fatal("expected HasCrawlerDB=true")
	}
	if resp.Counts.Retrieved != 1 || resp.Counts.Failed != 1 || resp.Counts.Future != 1 {
		t.Fatalf("unexpected counts: %+v", resp.Counts)
	}

	// 5. Disable transfer by URL
	body, _ := json.Marshal(DisableTransferRequest{
		Site: "mysite.com",
		URL:  "http://deadhost.com/icon.ico",
	})
	req = httptest.NewRequest("POST", "/api/v1/sites/transfers/disable", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable transfer: %d", rec.Code)
	}

	// 6. Add ignore pattern
	body, _ = json.Marshal(IgnorePatternRequest{
		Site:    "mysite.com",
		Pattern: "*.deadhost.com",
	})
	req = httptest.NewRequest("POST", "/api/v1/sites/ignores", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("add ignore pattern: %d", rec.Code)
	}

	// 7. Verify ignore pattern listed
	req = httptest.NewRequest("GET", "/api/v1/sites/ignores?site=mysite.com", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list ignore patterns: %d", rec.Code)
	}

	// 8. Remove ignore pattern
	req = httptest.NewRequest("DELETE", "/api/v1/sites/ignores?site=mysite.com&pattern=*.deadhost.com", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove ignore pattern: %d", rec.Code)
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

func TestServerSubmitDeduplication(t *testing.T) {
	srv, q := newTestServer(t)
	ctx := context.Background()

	// Pause queue initially so we can inspect queued state deterministically
	if err := q.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer q.Stop()

	submit := func(payload map[string]any) (*httptest.ResponseRecorder, Job) {
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		var j Job
		_ = json.Unmarshal(rec.Body.Bytes(), &j)
		return rec, j
	}

	// 1. First submit -> 201 Created, ID 1
	rec, j1 := submit(map[string]any{
		"source": "https://sourceforge.net/projects/dedup-srv/",
	})
	if rec.Code != http.StatusCreated || j1.ID != 1 || j1.SubmitOutcome != OutcomeCreated {
		t.Fatalf("expected 201 Created #1, got code=%d job=%+v", rec.Code, j1)
	}

	// 2. Duplicate submit while queued (with whitespace & explicit trailing-slash dest_dir) -> 200 OK, ID 1
	rec, jDup := submit(map[string]any{
		"source":   "  https://sourceforge.net/projects/dedup-srv/  ",
		"dest_dir": "/tmp/default_dest/",
	})
	if rec.Code != http.StatusOK || jDup.ID != j1.ID || jDup.SubmitOutcome != OutcomeExisting || jDup.State != StateQueued {
		t.Fatalf("expected 200 OK OutcomeExisting #1 queued, got code=%d job=%+v", rec.Code, jDup)
	}

	// 3. Resume queue so job #1 runs to completion
	if err := q.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	for range 50 {
		got, _ := q.GetJob(ctx, j1.ID)
		if got != nil && got.State == StateCompleted {
			break
		}
		// brief yield via status call
		_, _ = q.GetStatus(ctx)
	}
	// Pause again before testing re-submit
	_ = q.Pause(ctx)
	_ = q.store.FinishJob(ctx, j1.ID, StateCompleted, "")

	// 4. Duplicate submit when completed -> 200 OK, OutcomeExisting, still completed, still only 1 job total
	rec, jFin := submit(map[string]any{
		"source": "https://sourceforge.net/projects/dedup-srv/",
	})
	if rec.Code != http.StatusOK || jFin.ID != j1.ID || jFin.SubmitOutcome != OutcomeExisting || jFin.State != StateCompleted {
		t.Fatalf("expected 200 OK OutcomeExisting #1 completed, got code=%d job=%+v", rec.Code, jFin)
	}

	// 5. Force submit when completed -> 200 OK, OutcomeRequeued, reuses ID 1
	rec, jForce := submit(map[string]any{
		"source": "https://sourceforge.net/projects/dedup-srv/",
		"force":  true,
	})
	if rec.Code != http.StatusOK || jForce.ID != j1.ID || jForce.SubmitOutcome != OutcomeRequeued || jForce.State != StateQueued {
		t.Fatalf("expected 200 OK OutcomeRequeued #1 queued, got code=%d job=%+v", rec.Code, jForce)
	}

	jobs, err := q.ListJobs(ctx, JobFilter{})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("expected exactly 1 job in store, got %d (err=%v)", len(jobs), err)
	}
}
