package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rpajarola/mirrortools/git"
	"github.com/rpajarola/mirrortools/internal/config"
	"github.com/rpajarola/mirrortools/mirror"
)

// Server provides the REST API and Web UI HTTP handler.
type Server struct {
	queue      *Queue
	cfg        *config.Config
	configPath string
	webFS      fs.FS
	auth       *AuthManager
	mux        *http.ServeMux

	mu       sync.RWMutex
	eventSub map[chan string]struct{}
}

// NewServer creates a new API and Web server.
func NewServer(queue *Queue, cfg *config.Config, configPath string, webFS fs.FS) *Server {
	auth, _ := NewAuthManager(cfg.Daemon.Auth)
	s := &Server{
		queue:      queue,
		cfg:        cfg,
		configPath: configPath,
		webFS:      webFS,
		auth:       auth,
		mux:        http.NewServeMux(),
		eventSub:   make(map[chan string]struct{}),
	}
	s.routes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	// Queue-level endpoints
	s.mux.HandleFunc("GET /api/v1/queue/status", s.handleQueueStatus)
	s.mux.HandleFunc("POST /api/v1/queue/pause", s.handleQueuePause)
	s.mux.HandleFunc("POST /api/v1/queue/resume", s.handleQueueResume)
	s.mux.HandleFunc("POST /api/v1/queue/concurrency", s.handleQueueConcurrency)

	// Job-level endpoints
	s.mux.HandleFunc("GET /api/v1/jobs", s.handleListJobs)
	s.mux.HandleFunc("POST /api/v1/jobs", s.handleSubmitJob)
	s.mux.HandleFunc("POST /api/v1/jobs/batch", s.handleBatchJobs)
	s.mux.HandleFunc("GET /api/v1/jobs/{id}", s.handleGetJob)
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", s.handleCancelJob)
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/pause", s.handlePauseJob)
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/resume", s.handleResumeJob)
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/retry", s.handleRetryJob)
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/quarantine", s.handleQuarantineJob)
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/unquarantine", s.handleUnquarantineJob)
	s.mux.HandleFunc("DELETE /api/v1/jobs/{id}", s.handleDeleteJob)
	s.mux.HandleFunc("GET /api/v1/jobs/{id}/logs", s.handleGetLogs)
	s.mux.HandleFunc("GET /api/v1/jobs/{id}/logs/stream", s.handleStreamLogs)

	// Real-time events & config
	s.mux.HandleFunc("GET /api/v1/events", s.handleStreamEvents)
	s.mux.HandleFunc("GET /api/v1/config", s.handleGetConfig)
	s.mux.HandleFunc("PUT /api/v1/config", s.handleSaveConfig)

	// Authentication endpoints
	s.mux.HandleFunc("GET /auth/login", s.handleLoginPage)
	s.mux.HandleFunc("POST /auth/login", s.handleLoginSubmit)
	s.mux.HandleFunc("GET /auth/oauth/start", s.handleOAuthStart)
	s.mux.HandleFunc("GET /auth/callback", s.handleOAuthCallback)
	s.mux.HandleFunc("POST /auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/v1/auth/me", s.handleAuthMe)

	// Web UI
	if s.webFS != nil {
		fileServer := http.FileServer(http.FS(s.webFS))
		s.mux.Handle("/", fileServer)
	} else {
		s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprint(w, `<!DOCTYPE html><html><head><title>mirrord</title></head><body><h1>mirrord service</h1><p>API running. Web UI not embedded.</p><p><a href="/api/v1/queue/status">Queue Status API</a></p></body></html>`)
				return
			}
			http.NotFound(w, r)
		})
	}
}

// JSON write helper
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func parseJobID(r *http.Request) (int64, error) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid job ID: %q", idStr)
	}
	return id, nil
}

func (s *Server) handleQueueStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.queue.GetStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleQueuePause(w http.ResponseWriter, r *http.Request) {
	if err := s.queue.Pause(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.broadcastEvent("queue:paused")
	writeJSON(w, http.StatusOK, map[string]string{"status": "paused"})
}

func (s *Server) handleQueueResume(w http.ResponseWriter, r *http.Request) {
	if err := s.queue.Resume(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.broadcastEvent("queue:resumed")
	writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
}

func (s *Server) handleQueueConcurrency(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Concurrency int `json:"concurrency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if payload.Concurrency < 1 {
		writeError(w, http.StatusBadRequest, "concurrency must be at least 1")
		return
	}
	if err := s.queue.SetConcurrency(r.Context(), payload.Concurrency); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.broadcastEvent(fmt.Sprintf("concurrency:%d", payload.Concurrency))
	writeJSON(w, http.StatusOK, map[string]any{"concurrency": payload.Concurrency})
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var filter JobFilter
	if statesStr := q.Get("state"); statesStr != "" {
		for _, st := range strings.Split(statesStr, ",") {
			st = strings.TrimSpace(st)
			if State(st).Valid() {
				filter.States = append(filter.States, State(st))
			}
		}
	}
	filter.Method = q.Get("method")
	filter.Site = q.Get("site")
	filter.Prefix = q.Get("prefix")
	filter.OrderBy = q.Get("order_by")
	if l := q.Get("limit"); l != "" {
		filter.Limit, _ = strconv.Atoi(l)
	}
	if o := q.Get("offset"); o != "" {
		filter.Offset, _ = strconv.Atoi(o)
	}

	jobs, err := s.queue.ListJobs(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if jobs == nil {
		jobs = []*Job{}
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) handleBatchJobs(w http.ResponseWriter, r *http.Request) {
	var req BatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Action == "" {
		writeError(w, http.StatusBadRequest, "missing action in batch request")
		return
	}
	result, err := s.queue.BatchJobs(r.Context(), &req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.broadcastEvent(fmt.Sprintf("jobs:batch:%s", req.Action))
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleSubmitJob(w http.ResponseWriter, r *http.Request) {
	var req SubmitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Auto-detect method if omitted
	if req.Method == "" {
		if m, ok := mirror.Detect(req.Source); ok {
			req.Method = m.Name
			if req.Method == "http" && git.ProbeHTTP(r.Context(), req.Source) {
				req.Method = "git"
			}
		} else {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("cannot auto-detect mirror method for source %q; specify method", req.Source))
			return
		}
	} else if _, ok := mirror.Get(req.Method); !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown mirror method %q", req.Method))
		return
	}

	// Auto-fill destination if omitted
	s.mu.RLock()
	defaultDest := s.cfg.DestDir
	mc := s.cfg.Methods[req.Method]
	var methodFlags map[string]string
	if mc != nil && len(mc.Flags) > 0 {
		methodFlags = make(map[string]string, len(mc.Flags))
		for k, v := range mc.Flags {
			methodFlags[k] = v
		}
	}
	s.mu.RUnlock()

	if req.DestDir == "" {
		if mc != nil && mc.DestDir != "" {
			req.DestDir = mc.DestDir
		} else if defaultDest != "" {
			req.DestDir = defaultDest
			m, _ := mirror.Get(req.Method)
			if m != nil && m.Subdir != nil {
				sub := m.Subdir(req.Source)
				if sub != "" && filepath.IsLocal(sub) {
					req.DestDir = filepath.Join(req.DestDir, sub)
				}
			}
		} else {
			writeError(w, http.StatusBadRequest, "no destination directory given and no default destdir configured")
			return
		}
	}

	// Merge default flags from method config if not explicitly set
	if len(methodFlags) > 0 {
		if req.Flags == nil {
			req.Flags = make(map[string]string, len(methodFlags))
		}
		for k, v := range methodFlags {
			if _, ok := req.Flags[k]; !ok {
				req.Flags[k] = v
			}
		}
	}

	job, err := s.queue.Enqueue(r.Context(), &req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.broadcastEvent(fmt.Sprintf("job:enqueued:%d", job.ID))
	writeJSON(w, http.StatusCreated, job)
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id, err := parseJobID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := s.queue.GetJob(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id, err := parseJobID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.queue.CancelJob(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.broadcastEvent(fmt.Sprintf("job:cancelled:%d", id))
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (s *Server) handlePauseJob(w http.ResponseWriter, r *http.Request) {
	id, err := parseJobID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.queue.PauseJob(r.Context(), id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcastEvent(fmt.Sprintf("job:paused:%d", id))
	writeJSON(w, http.StatusOK, map[string]string{"status": "paused"})
}

func (s *Server) handleResumeJob(w http.ResponseWriter, r *http.Request) {
	id, err := parseJobID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.queue.ResumeJob(r.Context(), id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcastEvent(fmt.Sprintf("job:resumed:%d", id))
	writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
}

func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	id, err := parseJobID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := s.queue.RetryJob(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.broadcastEvent(fmt.Sprintf("job:retried:%d", id))
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleQuarantineJob(w http.ResponseWriter, r *http.Request) {
	id, err := parseJobID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&payload)
	reason := payload.Reason
	if reason == "" {
		reason = "manual quarantine via API"
	}

	if err := s.queue.QuarantineJob(r.Context(), id, reason); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.broadcastEvent(fmt.Sprintf("job:quarantined:%d", id))
	writeJSON(w, http.StatusOK, map[string]string{"status": "quarantined"})
}

func (s *Server) handleUnquarantineJob(w http.ResponseWriter, r *http.Request) {
	id, err := parseJobID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.queue.UnquarantineJob(r.Context(), id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcastEvent(fmt.Sprintf("job:unquarantined:%d", id))
	writeJSON(w, http.StatusOK, map[string]string{"status": "unquarantined"})
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	id, err := parseJobID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.queue.store.DeleteJob(r.Context(), id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcastEvent(fmt.Sprintf("job:deleted:%d", id))
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	id, err := parseJobID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	logs, err := s.queue.GetLogs(r.Context(), id, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if logs == nil {
		logs = []*JobLog{}
	}
	writeJSON(w, http.StatusOK, logs)
}

func (s *Server) handleStreamLogs(w http.ResponseWriter, r *http.Request) {
	id, err := parseJobID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, recent := s.queue.LogHub().Subscribe(id, 100)
	defer s.queue.LogHub().Unsubscribe(id, ch)

	// Send recent logs first
	for _, entry := range recent {
		data, _ := json.Marshal(entry)
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(entry)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (s *Server) handleStreamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 10)
	s.mu.Lock()
	s.eventSub[ch] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.eventSub, ch)
		s.mu.Unlock()
		close(ch)
	}()

	fmt.Fprintf(w, "data: connected\n\n")
	flusher.Flush()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", ev)
			flusher.Flush()
		case <-ticker.C:
			// Periodic heartbeat & status sync
			st, err := s.queue.GetStatus(ctx)
			if err == nil {
				data, _ := json.Marshal(st)
				fmt.Fprintf(w, "event: status\ndata: %s\n\n", data)
				flusher.Flush()
			}
		}
	}
}

func (s *Server) broadcastEvent(ev string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for ch := range s.eventSub {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var rawContent string
	if s.configPath != "" {
		if b, err := os.ReadFile(s.configPath); err == nil {
			rawContent = string(b)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"path":   s.configPath,
		"raw":    rawContent,
		"config": s.cfg,
	})
}

func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Raw string `json:"raw"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Validate config by saving to a temp file and loading it
	tmpFile, err := os.CreateTemp("", "mirrord-config-*.toml")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(payload.Raw); err != nil {
		tmpFile.Close()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmpFile.Close()

	newCfg, err := config.Load(tmpFile.Name())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid configuration: "+err.Error())
		return
	}

	// Write to real config file
	if s.configPath == "" {
		writeError(w, http.StatusBadRequest, "no configuration path configured")
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.configPath), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(s.configPath, []byte(payload.Raw), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.mu.Lock()
	s.cfg = newCfg
	// Reload auth manager
	if newAuth, err := NewAuthManager(newCfg.Daemon.Auth); err == nil {
		s.auth = newAuth
	}
	s.mu.Unlock()

	s.broadcastEvent("config:updated")
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// Auth returns the AuthManager.
func (s *Server) Auth() *AuthManager {
	return s.auth
}

// AuthEnabled reports whether authentication is enabled.
func (s *Server) AuthEnabled() bool {
	return s.auth != nil && s.auth.Enabled()
}

// PasswordAuthEnabled reports whether password authentication is enabled.
func (s *Server) PasswordAuthEnabled() bool {
	return s.auth != nil && s.auth.PasswordEnabled()
}

// OAuthEnabled reports whether OAuth authentication is enabled.
func (s *Server) OAuthEnabled() bool {
	return s.auth != nil && s.auth.OAuthEnabled()
}

// WithAuth wraps the server handler with authentication enforcement for TCP listeners.
func (s *Server) WithAuth() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.AuthEnabled() {
			s.ServeHTTP(w, r)
			return
		}

		path := r.URL.Path
		// Public auth endpoints
		if path == "/auth/login" || path == "/auth/callback" || path == "/auth/oauth/start" {
			s.ServeHTTP(w, r)
			return
		}

		user, ok := s.auth.ValidateRequest(r)
		if !ok {
			if strings.HasPrefix(path, "/api/") || strings.Contains(r.Header.Get("Accept"), "application/json") {
				w.Header().Set("WWW-Authenticate", `Bearer realm="mirrord"`)
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
			return
		}

		ctx := context.WithValue(r.Context(), userContextKey{}, user)
		s.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil {
		if _, ok := s.auth.ValidateRequest(r); ok {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}

	errMsg := r.URL.Query().Get("error")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>mirrord — Log in</title>
  <style>
    :root {
      --bg: #0f172a;
      --surface: #1e293b;
      --border: #334155;
      --text: #f8fafc;
      --text-muted: #94a3b8;
      --primary: #38bdf8;
      --primary-hover: #0284c7;
      --danger: #ef4444;
      --radius: 8px;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      background: var(--bg);
      color: var(--text);
      display: flex;
      align-items: center;
      justify-content: center;
      min-height: 100vh;
      padding: 20px;
    }
    .login-card {
      background: var(--surface);
      border: 1px solid var(--border);
      border-radius: var(--radius);
      padding: 32px;
      width: 100%%;
      max-width: 400px;
      box-shadow: 0 10px 25px -5px rgba(0, 0, 0, 0.4);
    }
    .brand {
      text-align: center;
      margin-bottom: 24px;
    }
    .brand h1 { font-size: 24px; font-weight: 700; }
    .brand p { font-size: 13px; color: var(--text-muted); margin-top: 4px; }
    .alert {
      background: rgba(239, 68, 68, 0.2);
      border: 1px solid rgba(239, 68, 68, 0.4);
      color: #fca5a5;
      padding: 10px 14px;
      border-radius: var(--radius);
      font-size: 13px;
      margin-bottom: 20px;
    }
    input {
      width: 100%%;
      background: var(--bg);
      border: 1px solid var(--border);
      color: var(--text);
      padding: 10px 14px;
      border-radius: var(--radius);
      font-size: 14px;
      margin-bottom: 16px;
      outline: none;
    }
    input:focus { border-color: var(--primary); }
    .btn {
      width: 100%%;
      padding: 10px 14px;
      border-radius: var(--radius);
      font-size: 14px;
      font-weight: 600;
      cursor: pointer;
      text-align: center;
      display: block;
      text-decoration: none;
      border: none;
    }
    .btn-primary { background: var(--primary); color: #000; }
    .btn-primary:hover { background: var(--primary-hover); color: #fff; }
    .btn-oauth {
      background: #24292f;
      color: #fff;
      border: 1px solid var(--border);
      margin-top: 10px;
    }
    .btn-oauth:hover { background: #32383f; }
    .divider {
      text-align: center;
      margin: 18px 0;
      color: var(--text-muted);
      font-size: 12px;
      position: relative;
    }
    .divider::before, .divider::after {
      content: "";
      position: absolute;
      top: 50%%;
      width: 42%%;
      height: 1px;
      background: var(--border);
    }
    .divider::before { left: 0; }
    .divider::after { right: 0; }
  </style>
</head>
<body>
  <div class="login-card">
    <div class="brand">
      <h1>mirrord</h1>
      <p>Authentication Required</p>
    </div>
`)

	if errMsg != "" {
		fmt.Fprintf(w, `    <div class="alert">%s</div>\n`, html.EscapeString(errMsg))
	}

	hasPassword := s.PasswordAuthEnabled()
	hasOAuth := s.OAuthEnabled()

	if hasPassword {
		fmt.Fprintf(w, `    <form method="POST" action="/auth/login">
      <input type="password" name="password" placeholder="Enter password" required autofocus />
      <button type="submit" class="btn btn-primary">Log in with Password</button>
    </form>
`)
	}

	if hasPassword && hasOAuth {
		fmt.Fprint(w, `    <div class="divider">or</div>\n`)
	}

	if hasOAuth {
		providerName := "Single Sign-On"
		if s.auth != nil {
			providerName = s.auth.ProviderName()
		}
		fmt.Fprintf(w, `    <a href="/auth/oauth/start" class="btn btn-oauth">Log in with %s</a>\n`, html.EscapeString(providerName))
	}

	if !hasPassword && !hasOAuth {
		fmt.Fprint(w, `    <p style="text-align:center;color:var(--text-muted);font-size:13px;">No login methods are currently configured.</p>\n`)
	}

	fmt.Fprint(w, `  </div>
</body>
</html>`)
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || !s.auth.PasswordEnabled() {
		writeError(w, http.StatusBadRequest, "password authentication is not enabled")
		return
	}

	var password string
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var payload struct {
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		password = payload.Password
	} else {
		_ = r.ParseForm()
		password = r.FormValue("password")
	}

	if !s.auth.CheckPassword(password) {
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			writeError(w, http.StatusUnauthorized, "invalid password")
		} else {
			http.Redirect(w, r, "/auth/login?error=Invalid+password", http.StatusSeeOther)
		}
		return
	}

	cookie := s.auth.CreateSession("admin")
	http.SetCookie(w, cookie)

	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "user": "admin"})
	} else {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || !s.auth.OAuthEnabled() {
		http.Error(w, "OAuth is not configured", http.StatusBadRequest)
		return
	}

	url, err := s.auth.OAuthStartURL(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || !s.auth.OAuthEnabled() {
		http.Error(w, "OAuth is not configured", http.StatusBadRequest)
		return
	}

	user, err := s.auth.HandleOAuthCallback(r.Context(), r)
	if err != nil {
		http.Redirect(w, r, "/auth/login?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	cookie := s.auth.CreateSession(user)
	http.SetCookie(w, cookie)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil {
		if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
			s.auth.InvalidateSession(c.Value)
		}
		http.SetCookie(w, s.auth.LogoutCookie())
	}

	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
	} else {
		http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
	}
}

func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	user := ""
	authenticated := false
	if s.auth != nil {
		if u, ok := s.auth.ValidateRequest(r); ok {
			user = u
			authenticated = true
		}
	} else {
		user = "anonymous"
		authenticated = true
	}

	providerName := ""
	if s.auth != nil {
		providerName = s.auth.ProviderName()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":          s.AuthEnabled(),
		"authenticated":    authenticated,
		"user":             user,
		"password_enabled": s.PasswordAuthEnabled(),
		"oauth_enabled":    s.OAuthEnabled(),
		"provider_name":    providerName,
	})
}
