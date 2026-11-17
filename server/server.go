package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/miownag/agent-local-bridge/internal/bridge"
	"github.com/miownag/agent-local-bridge/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Config struct {
	PublicURL    string
	AdminSecret  string
	WorkerSecret string
	AllowLocal   bool
	JobTimeout   time.Duration
}
type App struct {
	cfg   Config
	store store.Store
	mux   *http.ServeMux
	mcp   http.Handler
}

func FromEnv() (*App, error) {
	c := Config{PublicURL: os.Getenv("BRIDGE_PUBLIC_URL"), AdminSecret: os.Getenv("BRIDGE_ADMIN_SECRET"), WorkerSecret: os.Getenv("BRIDGE_WORKER_TOKEN"), AllowLocal: os.Getenv("BRIDGE_DEV") == "1" && os.Getenv("VERCEL") == ""}
	var s store.Store
	endpoint, token := os.Getenv("UPSTASH_REDIS_REST_URL"), os.Getenv("UPSTASH_REDIS_REST_TOKEN")
	if endpoint == "" && token == "" {
		endpoint, token = os.Getenv("KV_REST_API_URL"), os.Getenv("KV_REST_API_TOKEN")
	}
	if endpoint != "" {
		var err error
		s, err = store.NewRedis(endpoint, token, os.Getenv("BRIDGE_NAMESPACE"))
		if err != nil {
			return nil, err
		}
	} else if c.AllowLocal {
		s = store.NewMemory()
	} else {
		return nil, errors.New("persistent Redis REST storage is required outside local development")
	}
	return New(c, s)
}

func New(c Config, s store.Store) (*App, error) {
	u, e := url.Parse(c.PublicURL)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("BRIDGE_PUBLIC_URL must be an origin without path, credentials, query or fragment")
	}
	if u.Scheme != "https" && !(c.AllowLocal && u.Scheme == "http" && loopback(u.Hostname())) {
		return nil, errors.New("HTTPS is required; HTTP is permitted only on loopback in local development")
	}
	if len(c.AdminSecret) < 32 || len(c.WorkerSecret) < 32 || equal(c.AdminSecret, c.WorkerSecret) {
		return nil, errors.New("admin and worker secrets must be distinct and at least 32 bytes each")
	}
	if c.JobTimeout == 0 {
		c.JobTimeout = 45 * time.Second
	}
	if c.JobTimeout < time.Millisecond || c.JobTimeout > 60*time.Second {
		return nil, errors.New("job timeout must be within 60 seconds")
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	a := &App{cfg: c, store: s, mux: http.NewServeMux()}
	ms := mcp.NewServer(&mcp.Implementation{Name: "agent-local-bridge", Version: "0.1.0"}, nil)
	closed, nonDestructive := false, false
	mcp.AddTool(ms, &mcp.Tool{Name: "bridge_status", Description: "Check whether the local test-directory worker is online.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closed}}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, bridge.Status, error) {
		v, e := a.status(ctx)
		return nil, v, e
	})
	for _, name := range []string{"list_files", "read_file", "write_file"} {
		description := map[string]string{"list_files": "List entries in a relative test-directory path. Use . for the root.", "read_file": "Read a UTF-8 file inside the selected test directory, up to 64 KiB.", "write_file": "Create a new UTF-8 file inside the test directory, up to 64 KiB. Never overwrites existing files."}[name]
		mcp.AddTool(ms, &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: name != "write_file", DestructiveHint: &nonDestructive, OpenWorldHint: &closed}}, func(ctx context.Context, _ *mcp.CallToolRequest, args bridge.Arguments) (*mcp.CallToolResult, any, error) {
			out, e := a.dispatch(ctx, name, args)
			return nil, out, e
		})
	}
	a.mcp = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return ms }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: bridge.MaxBodyBytes})
	a.routes()
	return a, nil
}
func loopback(h string) bool {
	return h == "localhost" || (net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback())
}
func random() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func hash(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }
func equal(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}
func encode(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return string(b)
}
func (a *App) save(ctx context.Context, key string, v any, ttl time.Duration) error {
	return a.store.Set(ctx, key, encode(v), ttl)
}
func (a *App) load(ctx context.Context, key string, v any) error {
	s, e := a.store.Get(ctx, key)
	if e != nil {
		return e
	}
	if s == "" {
		return errors.New("not found")
	}
	return json.Unmarshal([]byte(s), v)
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func failure(w http.ResponseWriter, status int, code string) {
	reply(w, status, map[string]string{"error": code})
}
func body(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, bridge.MaxBodyBytes)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		failure(w, 400, "invalid_request")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		failure(w, 400, "invalid_request")
		return false
	}
	return true
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Keep same-origin form POSTs attributable while withholding cross-origin referrers.
	w.Header().Set("Referrer-Policy", "same-origin")
	formTargets := "'self' https://claude.ai/api/mcp/auth_callback"
	if a.cfg.AllowLocal {
		formTargets += " http://localhost:* http://127.0.0.1:* http://[::1]:*"
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action "+formTargets+"; frame-ancestors 'none'; base-uri 'none'")
	// Vercel terminates TLS. All absolute links come from the configured origin.
	if origin := r.Header.Get("Origin"); origin != "" && origin != a.cfg.PublicURL {
		failure(w, 403, "origin_not_allowed")
		return
	}
	a.mux.ServeHTTP(w, r)
}
func (a *App) routes() {
	a.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { reply(w, 200, map[string]string{"status": "ok"}) })
	a.mux.HandleFunc("GET /{$}", a.dashboard)
	a.mux.HandleFunc("POST /admin/login", a.adminLogin)
	a.mux.HandleFunc("POST /admin/revoke", a.adminRevoke)
	a.mux.HandleFunc("POST /admin/pause", a.adminPause)
	a.mux.HandleFunc("POST /admin/resume", a.adminResume)
	a.mux.HandleFunc("GET /.well-known/oauth-authorization-server", a.authMetadata)
	a.mux.HandleFunc("GET /.well-known/oauth-protected-resource", a.resourceMetadata)
	a.mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", a.resourceMetadata)
	a.mux.HandleFunc("POST /oauth/register", a.register)
	a.mux.HandleFunc("GET /oauth/authorize", a.authorize)
	a.mux.HandleFunc("POST /oauth/approve", a.approve)
	a.mux.HandleFunc("POST /oauth/token", a.token)
	a.mux.HandleFunc("POST /oauth/revoke", a.revoke)
	a.mux.HandleFunc("/mcp", a.protectedMCP)
	a.mux.HandleFunc("POST /worker/heartbeat", a.heartbeat)
	a.mux.HandleFunc("POST /worker/poll", a.poll)
	a.mux.HandleFunc("POST /worker/result", a.result)
}
func (a *App) workerAuth(w http.ResponseWriter, r *http.Request) bool {
	if !equal(r.Header.Get("Authorization"), "Bearer "+a.cfg.WorkerSecret) {
		failure(w, 401, "invalid_worker_token")
		return false
	}
	v, e := a.store.Get(r.Context(), "paused")
	if e != nil {
		failure(w, 503, "storage_unavailable")
		return false
	}
	if v == "1" {
		failure(w, 403, "bridge_paused")
		return false
	}
	return true
}
func (a *App) heartbeat(w http.ResponseWriter, r *http.Request) {
	if !a.workerAuth(w, r) {
		return
	}
	var b bridge.Heartbeat
	if !body(w, r, &b) {
		return
	}
	if len(b.RootName) > 100 || b.RootName == "" || strings.ContainsAny(b.RootName, "/\\\x00\r\n") {
		failure(w, 400, "invalid_root_name")
		return
	}
	b.At = time.Now().Unix()
	if a.save(r.Context(), "heartbeat", b, 30*time.Second) != nil {
		failure(w, 503, "storage_unavailable")
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
func (a *App) status(ctx context.Context) (bridge.Status, error) {
	v, e := a.store.Get(ctx, "paused")
	if e != nil {
		return bridge.Status{}, e
	}
	if v == "1" {
		return bridge.Status{}, nil
	}
	s, e := a.store.Get(ctx, "heartbeat")
	if e != nil {
		return bridge.Status{}, e
	}
	if s == "" {
		return bridge.Status{}, nil
	}
	var b bridge.Heartbeat
	if e = json.Unmarshal([]byte(s), &b); e != nil {
		return bridge.Status{}, e
	}
	return bridge.Status{Online: time.Now().Unix()-b.At < 30, RootName: b.RootName}, nil
}
func (a *App) poll(w http.ResponseWriter, r *http.Request) {
	if !a.workerAuth(w, r) {
		return
	}
	id, e := a.store.Get(r.Context(), "slot")
	if e != nil {
		failure(w, 503, "storage_unavailable")
		return
	}
	if id == "" {
		w.WriteHeader(204)
		return
	}
	s, e := a.store.Take(r.Context(), "pending:"+id)
	if e != nil {
		failure(w, 503, "storage_unavailable")
		return
	}
	if s == "" {
		w.WriteHeader(204)
		return
	}
	var j bridge.Job
	if json.Unmarshal([]byte(s), &j) != nil || j.Deadline <= time.Now().Unix() {
		w.WriteHeader(204)
		return
	}
	a.event(r.Context(), j, "running")
	reply(w, 200, j)
}
func (a *App) result(w http.ResponseWriter, r *http.Request) {
	if !a.workerAuth(w, r) {
		return
	}
	var b bridge.Result
	if !body(w, r, &b) {
		return
	}
	var j bridge.Job
	if a.load(r.Context(), "job:"+b.ID, &j) != nil || !equal(j.Receipt, b.Receipt) {
		failure(w, 404, "unknown_job")
		return
	}
	if j.Deadline <= time.Now().Unix() {
		failure(w, 410, "job_expired")
		return
	}
	if len(b.Output) > 96*1024 || len(b.Error) > 160 || (len(b.Output) == 0 && b.Error == "") || (len(b.Output) > 0 && b.Error != "") {
		failure(w, 400, "invalid_result")
		return
	}
	_, e := a.store.SetNX(r.Context(), "result:"+b.ID, encode(b), 90*time.Second)
	if e != nil {
		failure(w, 503, "storage_unavailable")
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
func (a *App) dispatch(ctx context.Context, tool string, args bridge.Arguments) (any, error) {
	if len(args.Path) > 1024 || len(args.Content) > bridge.MaxFileBytes {
		return nil, errors.New("input exceeds size limit")
	}
	s, e := a.status(ctx)
	if e != nil {
		return nil, errors.New("storage unavailable")
	}
	if !s.Online {
		return nil, errors.New("worker offline or paused")
	}
	j := bridge.Job{ID: random(), Tool: tool, Arguments: args, Receipt: random(), Deadline: time.Now().Add(a.cfg.JobTimeout).Unix()}
	ok, e := a.store.SetNX(ctx, "slot", j.ID, a.cfg.JobTimeout+5*time.Second)
	if e != nil {
		return nil, errors.New("storage unavailable")
	}
	if !ok {
		return nil, errors.New("worker busy; retry after the current tool finishes")
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = a.store.DeleteIf(c, "slot", j.ID)
		_, _ = a.store.Take(c, "pending:"+j.ID)
		_, _ = a.store.Take(c, "job:"+j.ID)
		_, _ = a.store.Take(c, "result:"+j.ID)
	}()
	if e = a.save(ctx, "job:"+j.ID, j, 90*time.Second); e != nil {
		return nil, errors.New("storage unavailable")
	}
	if e = a.save(ctx, "pending:"+j.ID, j, a.cfg.JobTimeout); e != nil {
		return nil, errors.New("storage unavailable")
	}
	a.event(ctx, j, "pending")
	timer := time.NewTimer(a.cfg.JobTimeout)
	defer timer.Stop()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			a.event(context.WithoutCancel(ctx), j, "cancelled")
			return nil, errors.New("request cancelled; a claimed operation may already have completed")
		case <-timer.C:
			a.event(context.WithoutCancel(ctx), j, "timeout")
			return nil, errors.New("worker timed out; check the test directory before retrying a write")
		case <-tick.C:
			raw, err := a.store.Get(ctx, "result:"+j.ID)
			if err != nil {
				return nil, errors.New("storage unavailable")
			}
			if raw == "" {
				continue
			}
			var result bridge.Result
			if json.Unmarshal([]byte(raw), &result) != nil {
				return nil, errors.New("invalid worker result")
			}
			if result.Error != "" {
				a.event(ctx, j, "failed")
				return nil, errors.New(result.Error)
			}
			var out any
			if json.Unmarshal(result.Output, &out) != nil {
				return nil, errors.New("invalid worker output")
			}
			a.event(ctx, j, "completed")
			return out, nil
		}
	}
}
func (a *App) event(ctx context.Context, j bridge.Job, state string) {
	n, e := a.store.Increment(ctx, "event-sequence", 24*time.Hour)
	if e != nil {
		return
	}
	_ = a.save(ctx, fmt.Sprintf("event:%d", n%20), bridge.Event{ID: j.ID, Tool: j.Tool, State: state, At: time.Now().Unix()}, 24*time.Hour)
}
