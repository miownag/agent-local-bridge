package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/miownag/agent-local-bridge/internal/bridge"
)

type Config struct {
	ServerURL, Token, Root string
	AllowHTTP              bool
	PollInterval           time.Duration
	Logger                 *slog.Logger
}
type Worker struct {
	cfg    Config
	client *http.Client
	files  *Files
}

func New(c Config) (*Worker, error) {
	u, e := url.Parse(c.ServerURL)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("server must be an HTTPS origin without path or credentials")
	}
	local := u.Hostname() == "localhost"
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
		local = true
	}
	if u.Scheme != "https" && !(c.AllowHTTP && u.Scheme == "http" && local) {
		return nil, errors.New("HTTPS required; --allow-http supports loopback only")
	}
	if len(c.Token) < 32 {
		return nil, errors.New("BRIDGE_WORKER_TOKEN must have at least 32 bytes")
	}
	if c.PollInterval == 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.PollInterval < 100*time.Millisecond {
		return nil, errors.New("poll interval must be at least 100 ms")
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	c.ServerURL = strings.TrimRight(c.ServerURL, "/")
	f, e := OpenFiles(c.Root)
	if e != nil {
		return nil, e
	}
	return &Worker{cfg: c, files: f, client: &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are not permitted") }}}, nil
}
func (w *Worker) Close() error { return w.files.Close() }

type httpError struct{ status int }

func (e httpError) Error() string { return fmt.Sprintf("server returned HTTP %d", e.status) }
func (w *Worker) request(ctx context.Context, path string, input, output any) (int, error) {
	b, e := json.Marshal(input)
	if e != nil {
		return 0, e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", w.cfg.ServerURL+path, bytes.NewReader(b))
	if e != nil {
		return 0, e
	}
	req.Header.Set("Authorization", "Bearer "+w.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	r, e := w.client.Do(req)
	if e != nil {
		return 0, errors.New("cannot reach bridge server")
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode > 299 {
		return r.StatusCode, httpError{r.StatusCode}
	}
	if output != nil && r.StatusCode != 204 {
		data, e := io.ReadAll(io.LimitReader(r.Body, bridge.MaxBodyBytes+1))
		if e != nil || len(data) > bridge.MaxBodyBytes {
			return r.StatusCode, errors.New("invalid server response")
		}
		if json.Unmarshal(data, output) != nil {
			return r.StatusCode, errors.New("invalid server response")
		}
	}
	return r.StatusCode, nil
}
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (w *Worker) Run(ctx context.Context) error {
	w.cfg.Logger.Info("worker started", "directory", w.files.Name)
	lastHeartbeat := time.Time{}
	backoff := time.Second
	for ctx.Err() == nil {
		if time.Since(lastHeartbeat) > 10*time.Second {
			status, e := w.request(ctx, "/worker/heartbeat", bridge.Heartbeat{RootName: w.files.Name, Protocol: bridge.ProtocolVersion}, nil)
			if e != nil {
				if status == 401 || status == 403 {
					return errors.New("worker credential rejected or bridge paused")
				}
				w.cfg.Logger.Warn("heartbeat failed; reconnecting")
				if sleep(ctx, backoff) != nil {
					break
				}
				backoff = min(backoff*2, 15*time.Second)
				continue
			}
			lastHeartbeat = time.Now()
			backoff = time.Second
		}
		var job bridge.Job
		status, e := w.request(ctx, "/worker/poll", struct{}{}, &job)
		if e != nil {
			if status == 401 || status == 403 {
				return errors.New("worker credential rejected or bridge paused")
			}
			w.cfg.Logger.Warn("poll failed; reconnecting")
			if sleep(ctx, backoff) != nil {
				break
			}
			backoff = min(backoff*2, 15*time.Second)
			continue
		}
		backoff = time.Second
		if status == 204 {
			if sleep(ctx, w.cfg.PollInterval) != nil {
				break
			}
			continue
		}
		if job.ID == "" || job.Receipt == "" || len(job.ID) > 64 || len(job.Receipt) > 64 || job.Deadline > time.Now().Add(90*time.Second).Unix() {
			return errors.New("invalid task envelope")
		}
		jobCtx, cancel := context.WithDeadline(ctx, time.Unix(job.Deadline, 0))
		result := w.files.ExecuteContext(jobCtx, job)
		cancel()
		w.cfg.Logger.Info("task executed", "tool", job.Tool, "success", result.Error == "")
		// Retry delivery of the same result, never execute a claimed task twice.
		for time.Now().Unix() < job.Deadline && ctx.Err() == nil {
			status, e = w.request(ctx, "/worker/result", result, nil)
			if e == nil || status == 404 || status == 410 {
				break
			}
			if status == 401 || status == 403 {
				return errors.New("worker credential rejected or bridge paused")
			}
			if sleep(ctx, time.Second) != nil {
				break
			}
		}
	}
	return ctx.Err()
}
