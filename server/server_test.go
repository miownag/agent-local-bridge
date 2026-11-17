package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miownag/agent-local-bridge/internal/bridge"
	"github.com/miownag/agent-local-bridge/internal/store"
	"github.com/miownag/agent-local-bridge/internal/worker"
)

type fixture struct {
	url    string
	app    *App
	client *http.Client
	cfg    Config
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	var apps [2]*App
	var next atomic.Uint32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { apps[next.Add(1)%2].ServeHTTP(w, r) }))
	t.Cleanup(s.Close)
	c := Config{PublicURL: s.URL, AdminSecret: strings.Repeat("a", 40), WorkerSecret: strings.Repeat("w", 40), AllowLocal: true, JobTimeout: 4 * time.Second}
	shared := store.NewMemory()
	for i := range apps {
		var e error
		apps[i], e = New(c, shared)
		if e != nil {
			t.Fatal(e)
		}
	}
	return &fixture{s.URL, apps[0], &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, c}
}
func (f *fixture) request(t *testing.T, method, path string, data any, token string) (*http.Response, []byte) {
	t.Helper()
	var b io.Reader
	if data != nil {
		j, e := json.Marshal(data)
		if e != nil {
			t.Fatal(e)
		}
		b = bytes.NewReader(j)
	}
	r, e := http.NewRequest(method, f.url+path, b)
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-11-25")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	res, e := f.client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	out, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	return res, out
}
func (f *fixture) form(t *testing.T, path string, b url.Values, cookies []*http.Cookie) (*http.Response, []byte) {
	t.Helper()
	r, e := http.NewRequest("POST", f.url+path, strings.NewReader(b.Encode()))
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", f.url)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	res, e := f.client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	out, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	return res, out
}
func field(t *testing.T, b []byte, key string) string {
	t.Helper()
	m := regexp.MustCompile(`name="` + key + `" value="([^"]+)"`).FindSubmatch(b)
	if len(m) != 2 {
		t.Fatalf("missing %s in form", key)
	}
	return string(m[1])
}
func check(t *testing.T, res *http.Response, want int, b []byte) {
	t.Helper()
	if res.StatusCode != want {
		t.Fatalf("HTTP %d, want %d: %s", res.StatusCode, want, b)
	}
}
func jsonString(t *testing.T, b []byte, key string) string {
	t.Helper()
	var m map[string]any
	if e := json.Unmarshal(b, &m); e != nil {
		t.Fatal(e)
	}
	s, _ := m[key].(string)
	if s == "" {
		t.Fatalf("missing JSON field %s: %s", key, b)
	}
	return s
}

func (f *fixture) authCode(t *testing.T) (string, string, string) {
	t.Helper()
	redirect := "http://127.0.0.1:9876/callback"
	r, b := f.request(t, "POST", "/oauth/register", map[string]any{"client_name": "integration-test", "redirect_uris": []string{redirect}, "token_endpoint_auth_method": "none"}, "")
	check(t, r, 201, b)
	clientID := jsonString(t, b, "client_id")
	verifier := strings.Repeat("v", 64)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "state": {"expected-state"}, "resource": {f.url + "/mcp"}}
	r, b = f.request(t, "GET", "/oauth/authorize?"+q.Encode(), nil, "")
	check(t, r, 200, b)
	if r.Header.Get("Referrer-Policy") != "same-origin" {
		t.Fatal("browser form policy must preserve same-origin POST Origin")
	}
	if !strings.Contains(r.Header.Get("Content-Security-Policy"), "https://claude.ai/api/mcp/auth_callback") {
		t.Fatal("browser form policy blocks the Claude OAuth redirect")
	}
	cookies := r.Cookies()
	csrf := field(t, b, "csrf")
	consent := field(t, b, "consent")
	approval := url.Values{"secret": {f.cfg.AdminSecret}, "csrf": {csrf}, "consent": {consent}}
	bad := url.Values{"secret": {f.cfg.AdminSecret}, "csrf": {"wrong"}, "consent": {consent}}
	r, b = f.form(t, "/oauth/approve", bad, cookies)
	check(t, r, 403, b)
	r, b = f.form(t, "/oauth/approve", approval, cookies)
	check(t, r, 303, b)
	u, e := url.Parse(r.Header.Get("Location"))
	if e != nil {
		t.Fatal(e)
	}
	if u.Query().Get("state") != "expected-state" {
		t.Fatal("OAuth state lost")
	}
	return clientID, u.Query().Get("code"), verifier
}
func (f *fixture) tokens(t *testing.T) (string, string, string) {
	t.Helper()
	id, code, verifier := f.authCode(t)
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {id}, "code": {code}, "redirect_uri": {"http://127.0.0.1:9876/callback"}, "code_verifier": {verifier}, "resource": {f.url + "/mcp"}}
	bad := maps.Clone(form)
	bad.Set("code_verifier", strings.Repeat("x", 64))
	r, b := f.form(t, "/oauth/token", bad, nil)
	check(t, r, 400, b)
	r, b = f.form(t, "/oauth/token", form, nil)
	check(t, r, 200, b)
	access, refresh := jsonString(t, b, "access_token"), jsonString(t, b, "refresh_token")
	r, b = f.form(t, "/oauth/token", form, nil)
	check(t, r, 400, b)
	return access, refresh, id
}

func TestOAuthAndRevocation(t *testing.T) {
	f := newFixture(t)
	r, b := f.request(t, "POST", "/mcp", map[string]any{}, "")
	check(t, r, 401, b)
	if !strings.Contains(r.Header.Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource/mcp") {
		t.Fatal("resource discovery missing")
	}
	r, b = f.request(t, "POST", "/oauth/register", map[string]any{"redirect_uris": []string{"https://attacker.example/callback"}}, "")
	check(t, r, 400, b)
	access, refresh, id := f.tokens(t)
	r, b = f.request(t, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, access)
	check(t, r, 200, b)
	if !strings.Contains(string(b), "write_file") {
		t.Fatal(string(b))
	}
	refreshForm := url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "refresh_token": {refresh}, "resource": {f.url + "/mcp"}}
	r, b = f.form(t, "/oauth/token", refreshForm, nil)
	check(t, r, 200, b)
	nextAccess, nextRefresh := jsonString(t, b, "access_token"), jsonString(t, b, "refresh_token")
	r, b = f.form(t, "/oauth/token", refreshForm, nil)
	check(t, r, 400, b)
	r, b = f.form(t, "/oauth/revoke", url.Values{"client_id": {id}, "token": {nextRefresh}}, nil)
	check(t, r, 200, b)
	for _, token := range []string{access, nextAccess} {
		r, b = f.request(t, "POST", "/mcp", map[string]any{}, token)
		check(t, r, 401, b)
	}
}
func TestMCPWorkerAcrossInstances(t *testing.T) {
	f := newFixture(t)
	access, _, _ := f.tokens(t)
	dir := t.TempDir()
	nonce := random()
	if e := os.WriteFile(filepath.Join(dir, "challenge.txt"), []byte(nonce), 0600); e != nil {
		t.Fatal(e)
	}
	w, e := worker.New(worker.Config{ServerURL: f.url, Token: f.cfg.WorkerSecret, Root: dir, AllowHTTP: true, PollInterval: 100 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done; _ = w.Close() })
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, e := f.app.status(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if status.Online {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not become online")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, b := f.request(t, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "e2e", "version": "1"}}}, access)
	check(t, r, 200, b)
	call := func(name string, args map[string]any) []byte {
		r, b := f.request(t, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}, access)
		check(t, r, 200, b)
		return b
	}
	b = call("read_file", map[string]any{"path": "challenge.txt"})
	if !strings.Contains(string(b), nonce) {
		t.Fatalf("local nonce missing: %s", b)
	}
	b = call("write_file", map[string]any{"path": "created.txt", "content": "from-cloud"})
	if strings.Contains(string(b), `"isError":true`) {
		t.Fatal(string(b))
	}
	data, e := os.ReadFile(filepath.Join(dir, "created.txt"))
	if e != nil || string(data) != "from-cloud" {
		t.Fatalf("file not created: %s %v", data, e)
	}
	b = call("write_file", map[string]any{"path": "created.txt", "content": "overwrite"})
	if !strings.Contains(string(b), `"isError":true`) {
		t.Fatalf("overwrite not rejected: %s", b)
	}
	b = call("read_file", map[string]any{"path": "../outside"})
	if !strings.Contains(string(b), `"isError":true`) {
		t.Fatalf("traversal not rejected: %s", b)
	}
	b = call("list_files", map[string]any{"path": "."})
	if !strings.Contains(string(b), "challenge.txt") {
		t.Fatalf("directory entries missing: %s", b)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "created.txt"))
	if string(data) != "from-cloud" {
		t.Fatal("existing file was changed")
	}
}
func TestWorkerAuthAndClaimOnce(t *testing.T) {
	f := newFixture(t)
	r, b := f.request(t, "POST", "/worker/poll", struct{}{}, "bad")
	check(t, r, 401, b)
	j := bridge.Job{ID: "test", Receipt: "receipt", Deadline: time.Now().Add(time.Minute).Unix()}
	ctx := context.Background()
	if e := f.app.store.Set(ctx, "slot", j.ID, time.Minute); e != nil {
		t.Fatal(e)
	}
	_ = f.app.save(ctx, "pending:"+j.ID, j, time.Minute)
	_ = f.app.save(ctx, "job:"+j.ID, j, time.Minute)
	r, b = f.request(t, "POST", "/worker/poll", struct{}{}, f.cfg.WorkerSecret)
	check(t, r, 200, b)
	r, b = f.request(t, "POST", "/worker/poll", struct{}{}, f.cfg.WorkerSecret)
	check(t, r, 204, b)
	r, b = f.request(t, "POST", "/worker/result", bridge.Result{ID: j.ID, Receipt: "wrong", Output: json.RawMessage(`{}`)}, f.cfg.WorkerSecret)
	check(t, r, 404, b)
	for _, value := range []string{`{"first":true}`, `{"second":true}`} {
		r, b = f.request(t, "POST", "/worker/result", bridge.Result{ID: j.ID, Receipt: j.Receipt, Output: json.RawMessage(value)}, f.cfg.WorkerSecret)
		check(t, r, 200, b)
	}
	raw, _ := f.app.store.Get(ctx, "result:"+j.ID)
	if strings.Contains(raw, "second") {
		t.Fatal("duplicate result overwrote the original")
	}
}
func TestOfflineTimeoutAndRevokeAll(t *testing.T) {
	f := newFixture(t)
	_, e := f.app.dispatch(context.Background(), "read_file", bridge.Arguments{Path: "x"})
	if e == nil || !strings.Contains(e.Error(), "offline") {
		t.Fatal(e)
	}
	f.app.cfg.JobTimeout = 100 * time.Millisecond
	_ = f.app.save(context.Background(), "heartbeat", bridge.Heartbeat{RootName: "test", At: time.Now().Unix()}, time.Minute)
	_, e = f.app.dispatch(context.Background(), "read_file", bridge.Arguments{Path: "x"})
	if e == nil || !strings.Contains(e.Error(), "timed out") {
		t.Fatal(e)
	}
	v, _ := f.app.store.Get(context.Background(), "slot")
	if v != "" {
		t.Fatal("timed-out slot was not released")
	}
	access, _, _ := f.tokens(t)
	r, b := f.request(t, "GET", "/", nil, "")
	check(t, r, 200, b)
	cookies := r.Cookies()
	csrf := field(t, b, "csrf")
	r, b = f.form(t, "/admin/login", url.Values{"secret": {f.cfg.AdminSecret}, "csrf": {csrf}}, cookies)
	check(t, r, 303, b)
	cookies = append(cookies, r.Cookies()...)
	r, b = f.form(t, "/admin/revoke", url.Values{"csrf": {csrf}}, cookies)
	check(t, r, 303, b)
	r, b = f.request(t, "POST", "/mcp", map[string]any{}, access)
	check(t, r, 401, b)
	r, b = f.form(t, "/admin/pause", url.Values{"csrf": {csrf}}, cookies)
	check(t, r, 303, b)
	r, b = f.request(t, "POST", "/worker/poll", struct{}{}, f.cfg.WorkerSecret)
	check(t, r, 403, b)
}
func TestConfigFailsClosed(t *testing.T) {
	c := Config{PublicURL: "http://public.example", AdminSecret: strings.Repeat("a", 40), WorkerSecret: strings.Repeat("w", 40), AllowLocal: true}
	if _, e := New(c, store.NewMemory()); e == nil {
		t.Fatal("public HTTP allowed")
	}
	t.Setenv("VERCEL", "1")
	t.Setenv("BRIDGE_DEV", "1")
	t.Setenv("UPSTASH_REDIS_REST_URL", "")
	if _, e := FromEnv(); e == nil {
		t.Fatal("Vercel accepted in-memory storage")
	}
}

func TestMarketplaceRedisEnvironment(t *testing.T) {
	t.Setenv("VERCEL", "1")
	t.Setenv("BRIDGE_PUBLIC_URL", "https://bridge.example")
	t.Setenv("BRIDGE_ADMIN_SECRET", strings.Repeat("a", 40))
	t.Setenv("BRIDGE_WORKER_TOKEN", strings.Repeat("w", 40))
	t.Setenv("BRIDGE_NAMESPACE", "marketplace-test")
	t.Setenv("UPSTASH_REDIS_REST_URL", "")
	t.Setenv("UPSTASH_REDIS_REST_TOKEN", "")
	t.Setenv("KV_REST_API_URL", "https://redis.example")
	t.Setenv("KV_REST_API_TOKEN", "marketplace-token")
	app, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := app.store.(*store.Redis); !ok {
		t.Fatal("Marketplace Redis was not selected")
	}
	t.Setenv("UPSTASH_REDIS_REST_URL", "https://explicit.example")
	if _, err = FromEnv(); err == nil {
		t.Fatal("partially configured explicit credentials were mixed with Marketplace credentials")
	}
}

func TestUntrustedBrowserOriginsRemainBlocked(t *testing.T) {
	f := newFixture(t)
	for _, origin := range []string{"null", "https://attacker.example"} {
		r, err := http.NewRequest("POST", f.url+"/oauth/approve", strings.NewReader(""))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Origin", origin)
		res, err := f.client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 403 {
			t.Fatalf("untrusted Origin %q returned %d", origin, res.StatusCode)
		}
	}
}
