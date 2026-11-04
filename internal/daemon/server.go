package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rpajarola/mirrortools/internal/config"
	"github.com/rpajarola/mirrortools/mirror"
)

// Server provides the REST API and Web UI HTTP handler.
type Server struct {
	queue      *Queue
	cfg        *config.Config
	configPath string
	webFS      fs.FS
	mux        *http.ServeMux

	mu       sync.RWMutex
	eventSub map[chan string]struct{}
}

// NewServer creates a new API and Web server.
func NewServer(queue *Queue, cfg *config.Config, configPath string, webFS fs.FS) *Server {
	s := &Server{
		queue:      queue,
		cfg:        cfg,
		configPath: configPath,
		webFS:      webFS,
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
		} else {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("cannot auto-detect mirror method for source %q; specify method", req.Source))
			return
		}
	} else if _, ok := mirror.Get(req.Method); !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown mirror method %q", req.Method))
		return
	}

	// Auto-fill destination if omitted
	if req.DestDir == "" {
		s.mu.RLock()
		defaultDest := s.cfg.DestDir
		mc := s.cfg.Methods[req.Method]
		s.mu.RUnlock()

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
	s.mu.Unlock()

	s.broadcastEvent("config:updated")
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}
