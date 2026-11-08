package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/rpajarola/mirrortools/httpmirror"
)

// SiteTransfersResponse carries transfers and metadata for a site.
type SiteTransfersResponse struct {
	Site         string                    `json:"site"`
	Method       string                    `json:"method"`
	DestDir      string                    `json:"dest_dir"`
	HasCrawlerDB bool                      `json:"has_crawler_db"`
	Counts       httpmirror.TransferCounts `json:"counts"`
	Transfers    []*httpmirror.Transfer    `json:"transfers"`
	TotalMatches int                       `json:"total_matches"`
	Ignores      []*httpmirror.IgnoreRule  `json:"ignores"`
}

// DisableTransferRequest specifies a transfer URL or pattern to disable.
type DisableTransferRequest struct {
	Site    string `json:"site"`
	URL     string `json:"url,omitempty"`
	Pattern string `json:"pattern,omitempty"`
}

// EnableTransferRequest specifies a disabled transfer URL to re-enable.
type EnableTransferRequest struct {
	Site string `json:"site"`
	URL  string `json:"url"`
}

// IgnorePatternRequest specifies an ignore pattern to add or remove for a site.
type IgnorePatternRequest struct {
	Site    string `json:"site"`
	Pattern string `json:"pattern"`
}

func parseSiteQuery(r *http.Request) string {
	site := r.PathValue("site")
	if site == "" {
		site = r.URL.Query().Get("site")
	}
	return strings.TrimSpace(site)
}

func (s *Server) handleGetSiteTransfers(w http.ResponseWriter, r *http.Request) {
	site := parseSiteQuery(r)
	if site == "" {
		writeError(w, http.StatusBadRequest, "missing site parameter")
		return
	}

	siteInfo, err := s.queue.store.GetSiteInfo(r.Context(), site)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	q := r.URL.Query()
	category := q.Get("category")
	if category == "" {
		category = q.Get("state")
	}
	search := q.Get("search")
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	offset, _ := strconv.Atoi(q.Get("offset"))

	resp := &SiteTransfersResponse{
		Site:    siteInfo.Site,
		Method:  siteInfo.Method,
		DestDir: siteInfo.DestDir,
	}

	if httpmirror.DBExists(siteInfo.DestDir) {
		resp.HasCrawlerDB = true
		counts, err := httpmirror.GetTransferCounts(siteInfo.DestDir)
		if err == nil && counts != nil {
			resp.Counts = *counts
		}
		transfers, total, err := httpmirror.ListTransfers(siteInfo.DestDir, category, search, limit, offset)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp.Transfers = transfers
		resp.TotalMatches = total

		ignores, _ := httpmirror.ListIgnorePatterns(siteInfo.DestDir)
		resp.Ignores = ignores
	} else {
		// Fallback: populate transfers from daemon jobs for this site
		resp.HasCrawlerDB = false
		jobs, err := s.queue.store.ListJobs(r.Context(), JobFilter{Site: siteInfo.Site})
		if err == nil {
			for _, j := range jobs {
				cat := "future"
				st := httpmirror.TransferPending
				switch j.State {
				case StateCompleted:
					cat = "retrieved"
					st = httpmirror.TransferDone
					resp.Counts.Retrieved++
				case StateFailed:
					cat = "failed"
					st = httpmirror.TransferFailed
					resp.Counts.Failed++
				case StatePaused, StateQuarantined:
					cat = "disabled"
					st = httpmirror.TransferDisabled
					resp.Counts.Disabled++
				default:
					cat = "future"
					st = httpmirror.TransferPending
					resp.Counts.Future++
				}
				resp.Counts.Total++

				if category != "" && category != "all" && category != cat {
					continue
				}
				if search != "" && !strings.Contains(strings.ToLower(j.Source), strings.ToLower(search)) {
					continue
				}

				resp.Transfers = append(resp.Transfers, &httpmirror.Transfer{
					ID:        j.ID,
					URL:       j.Source,
					State:     st,
					Category:  cat,
					Error:     j.LastError,
					Requisite: false,
				})
			}
			resp.TotalMatches = len(resp.Transfers)
			if offset < len(resp.Transfers) {
				end := offset + limit
				if end > len(resp.Transfers) {
					end = len(resp.Transfers)
				}
				resp.Transfers = resp.Transfers[offset:end]
			} else {
				resp.Transfers = []*httpmirror.Transfer{}
			}
		}
	}

	if resp.Transfers == nil {
		resp.Transfers = []*httpmirror.Transfer{}
	}
	if resp.Ignores == nil {
		resp.Ignores = []*httpmirror.IgnoreRule{}
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDisableSiteTransfer(w http.ResponseWriter, r *http.Request) {
	var req DisableTransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Site == "" {
		req.Site = parseSiteQuery(r)
	}
	if req.Site == "" {
		writeError(w, http.StatusBadRequest, "missing site")
		return
	}
	if req.URL == "" && req.Pattern == "" {
		writeError(w, http.StatusBadRequest, "specify url or pattern to disable")
		return
	}

	siteInfo, err := s.queue.store.GetSiteInfo(r.Context(), req.Site)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if httpmirror.DBExists(siteInfo.DestDir) {
		if req.Pattern != "" {
			if err := httpmirror.AddIgnorePattern(siteInfo.DestDir, req.Pattern); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		if req.URL != "" {
			if err := httpmirror.DisableTransfer(siteInfo.DestDir, req.URL); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}

	// Also pause/quarantine any active or queued job for this URL/pattern in daemon queue
	target := req.URL
	if target == "" {
		target = req.Pattern
	}
	_, _ = s.queue.BatchJobs(r.Context(), &BatchRequest{
		Action: BatchPause,
		Prefix: target,
	})

	s.broadcastEvent(fmt.Sprintf("transfers:disabled:%s", req.Site))
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "disabled",
		"site":    req.Site,
		"url":     req.URL,
		"pattern": req.Pattern,
	})
}

func (s *Server) handleEnableSiteTransfer(w http.ResponseWriter, r *http.Request) {
	var req EnableTransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Site == "" {
		req.Site = parseSiteQuery(r)
	}
	if req.Site == "" || req.URL == "" {
		writeError(w, http.StatusBadRequest, "missing site or url")
		return
	}

	siteInfo, err := s.queue.store.GetSiteInfo(r.Context(), req.Site)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if httpmirror.DBExists(siteInfo.DestDir) {
		if err := httpmirror.EnableTransfer(siteInfo.DestDir, req.URL); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	// In daemon queue, unpause if paused
	_, _ = s.queue.BatchJobs(r.Context(), &BatchRequest{
		Action: BatchResume,
		Prefix: req.URL,
	})

	s.broadcastEvent(fmt.Sprintf("transfers:enabled:%s", req.Site))
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "enabled",
		"site":   req.Site,
		"url":    req.URL,
	})
}

func (s *Server) handleListSiteIgnores(w http.ResponseWriter, r *http.Request) {
	site := parseSiteQuery(r)
	if site == "" {
		writeError(w, http.StatusBadRequest, "missing site parameter")
		return
	}

	siteInfo, err := s.queue.store.GetSiteInfo(r.Context(), site)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if !httpmirror.DBExists(siteInfo.DestDir) {
		writeJSON(w, http.StatusOK, []*httpmirror.IgnoreRule{})
		return
	}

	rules, err := httpmirror.ListIgnorePatterns(siteInfo.DestDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rules == nil {
		rules = []*httpmirror.IgnoreRule{}
	}
	writeJSON(w, http.StatusOK, rules)
}

func (s *Server) handleAddSiteIgnore(w http.ResponseWriter, r *http.Request) {
	var req IgnorePatternRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Site == "" {
		req.Site = parseSiteQuery(r)
	}
	if req.Site == "" || req.Pattern == "" {
		writeError(w, http.StatusBadRequest, "missing site or pattern")
		return
	}

	siteInfo, err := s.queue.store.GetSiteInfo(r.Context(), req.Site)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if httpmirror.DBExists(siteInfo.DestDir) {
		if err := httpmirror.AddIgnorePattern(siteInfo.DestDir, req.Pattern); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	// Also pause jobs matching pattern in daemon queue
	_, _ = s.queue.BatchJobs(r.Context(), &BatchRequest{
		Action: BatchPause,
		Prefix: req.Pattern,
	})

	s.broadcastEvent(fmt.Sprintf("transfers:ignores:%s", req.Site))
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "added",
		"site":    req.Site,
		"pattern": req.Pattern,
	})
}

func (s *Server) handleRemoveSiteIgnore(w http.ResponseWriter, r *http.Request) {
	site := parseSiteQuery(r)
	pattern := r.URL.Query().Get("pattern")
	if pattern == "" {
		var req IgnorePatternRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			if req.Site != "" {
				site = req.Site
			}
			if req.Pattern != "" {
				pattern = req.Pattern
			}
		}
	}

	if site == "" || pattern == "" {
		writeError(w, http.StatusBadRequest, "missing site or pattern")
		return
	}

	siteInfo, err := s.queue.store.GetSiteInfo(r.Context(), site)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if httpmirror.DBExists(siteInfo.DestDir) {
		if err := httpmirror.RemoveIgnorePattern(siteInfo.DestDir, pattern); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	s.broadcastEvent(fmt.Sprintf("transfers:ignores:%s", site))
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "removed",
		"site":    site,
		"pattern": pattern,
	})
}
