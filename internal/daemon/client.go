package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is an HTTP client that communicates with mirrord over a Unix domain socket or TCP.
type Client struct {
	http   *http.Client
	target string // "http://unix" for socket, or "http://host:port"
}

// NewUnixClient creates a client connecting to mirrord over a Unix domain socket.
func NewUnixClient(socketPath string) *Client {
	return &Client{
		target: "http://unix",
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// NewHTTPClient creates a client connecting to mirrord over TCP HTTP.
func NewHTTPClient(baseURL string) *Client {
	baseURL = strings.TrimSuffix(baseURL, "/")
	return &Client{
		target: baseURL,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) url(endpoint string) string {
	return c.target + endpoint
}

func checkErr(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	defer resp.Body.Close()
	var errResp struct {
		Error string `json:"error"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error != "" {
		return fmt.Errorf("server error: %s", errResp.Error)
	}
	return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// GetStatus returns the current queue metrics.
func (c *Client) GetStatus(ctx context.Context) (*QueueStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/api/v1/queue/status"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkErr(resp); err != nil {
		return nil, err
	}

	var status QueueStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, err
	}
	return &status, nil
}

// PauseQueue pauses queue processing.
func (c *Client) PauseQueue(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/api/v1/queue/pause"), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkErr(resp)
}

// ResumeQueue resumes queue processing.
func (c *Client) ResumeQueue(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/api/v1/queue/resume"), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkErr(resp)
}

// SetConcurrency updates the worker count.
func (c *Client) SetConcurrency(ctx context.Context, n int) error {
	body, _ := json.Marshal(map[string]int{"concurrency": n})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/api/v1/queue/concurrency"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkErr(resp)
}

// ListJobs queries jobs.
func (c *Client) ListJobs(ctx context.Context, filter JobFilter) ([]*Job, error) {
	vals := url.Values{}
	if len(filter.States) > 0 {
		var sts []string
		for _, s := range filter.States {
			sts = append(sts, string(s))
		}
		vals.Set("state", strings.Join(sts, ","))
	}
	if filter.Method != "" {
		vals.Set("method", filter.Method)
	}
	if filter.Site != "" {
		vals.Set("site", filter.Site)
	}
	if filter.Prefix != "" {
		vals.Set("prefix", filter.Prefix)
	}
	if filter.OrderBy != "" {
		vals.Set("order_by", filter.OrderBy)
	}
	if filter.Limit > 0 {
		vals.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		vals.Set("offset", strconv.Itoa(filter.Offset))
	}

	endpoint := "/api/v1/jobs"
	if len(vals) > 0 {
		endpoint += "?" + vals.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(endpoint), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkErr(resp); err != nil {
		return nil, err
	}

	var jobs []*Job
	if err := json.NewDecoder(resp.Body).Decode(&jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

// SubmitJob enqueues a new mirror job.
func (c *Client) SubmitJob(ctx context.Context, sub *SubmitRequest) (*Job, error) {
	body, _ := json.Marshal(sub)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/api/v1/jobs"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkErr(resp); err != nil {
		return nil, err
	}

	var job Job
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return nil, err
	}
	return &job, nil
}

// GetJob fetches job details.
func (c *Client) GetJob(ctx context.Context, id int64) (*Job, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(fmt.Sprintf("/api/v1/jobs/%d", id)), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkErr(resp); err != nil {
		return nil, err
	}

	var job Job
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return nil, err
	}
	return &job, nil
}

// CancelJob cancels a job.
func (c *Client) CancelJob(ctx context.Context, id int64) error {
	return c.postAction(ctx, fmt.Sprintf("/api/v1/jobs/%d/cancel", id), nil)
}

// PauseJob pauses a job.
func (c *Client) PauseJob(ctx context.Context, id int64) error {
	return c.postAction(ctx, fmt.Sprintf("/api/v1/jobs/%d/pause", id), nil)
}

// ResumeJob resumes a job.
func (c *Client) ResumeJob(ctx context.Context, id int64) error {
	return c.postAction(ctx, fmt.Sprintf("/api/v1/jobs/%d/resume", id), nil)
}

// RetryJob retries a job.
func (c *Client) RetryJob(ctx context.Context, id int64) (*Job, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(fmt.Sprintf("/api/v1/jobs/%d/retry", id)), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkErr(resp); err != nil {
		return nil, err
	}
	var job Job
	_ = json.NewDecoder(resp.Body).Decode(&job)
	return &job, nil
}

// QuarantineJob quarantines a job.
func (c *Client) QuarantineJob(ctx context.Context, id int64, reason string) error {
	body, _ := json.Marshal(map[string]string{"reason": reason})
	return c.postAction(ctx, fmt.Sprintf("/api/v1/jobs/%d/quarantine", id), bytes.NewReader(body))
}

// UnquarantineJob unquarantines a job.
func (c *Client) UnquarantineJob(ctx context.Context, id int64) error {
	return c.postAction(ctx, fmt.Sprintf("/api/v1/jobs/%d/unquarantine", id), nil)
}

// DeleteJob permanently deletes a finished job.
func (c *Client) DeleteJob(ctx context.Context, id int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url(fmt.Sprintf("/api/v1/jobs/%d", id)), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkErr(resp)
}

// BatchJobs applies a batch operation (pause, resume, quarantine, unquarantine, cancel, retry, delete) by prefix or IDs.
func (c *Client) BatchJobs(ctx context.Context, req *BatchRequest) (*BatchResult, error) {
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/api/v1/jobs/batch"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkErr(resp); err != nil {
		return nil, err
	}

	var res BatchResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return &res, nil
}

// GetLogs returns stored logs for a job.
func (c *Client) GetLogs(ctx context.Context, id int64, limit, offset int) ([]*JobLog, error) {
	vals := url.Values{}
	if limit > 0 {
		vals.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		vals.Set("offset", strconv.Itoa(offset))
	}
	endpoint := fmt.Sprintf("/api/v1/jobs/%d/logs", id)
	if len(vals) > 0 {
		endpoint += "?" + vals.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(endpoint), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkErr(resp); err != nil {
		return nil, err
	}

	var logs []*JobLog
	if err := json.NewDecoder(resp.Body).Decode(&logs); err != nil {
		return nil, err
	}
	return logs, nil
}

// StreamLogs connects to the SSE log stream for a job.
func (c *Client) StreamLogs(ctx context.Context, id int64, onLog func(l *JobLog)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(fmt.Sprintf("/api/v1/jobs/%d/logs/stream", id)), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkErr(resp); err != nil {
		return err
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var l JobLog
			if err := json.Unmarshal([]byte(data), &l); err == nil && onLog != nil {
				onLog(&l)
			}
		}
	}
	return scanner.Err()
}

func (c *Client) postAction(ctx context.Context, endpoint string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(endpoint), body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkErr(resp)
}
