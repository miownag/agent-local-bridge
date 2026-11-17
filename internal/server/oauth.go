package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const oauthScope = "bridge:files"
const grantTTL = 8 * 24 * time.Hour

type oauthClient struct {
	ID         string   `json:"client_id"`
	Name       string   `json:"client_name"`
	Redirects  []string `json:"redirect_uris"`
	Method     string   `json:"token_endpoint_auth_method"`
	SecretHash string   `json:"secret_hash,omitempty"`
}
type authorization struct {
	ClientID  string
	Redirect  string
	Challenge string
	State     string
	CSRFHash  string
}
type accessGrant struct {
	ClientID string
	GrantID  string
	Resource string
	Scope    string
}

var pkcePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43,128}$`)

func (a *App) authMetadata(w http.ResponseWriter, r *http.Request) {
	b := a.cfg.PublicURL
	reply(w, 200, map[string]any{"issuer": b, "authorization_endpoint": b + "/oauth/authorize", "token_endpoint": b + "/oauth/token", "registration_endpoint": b + "/oauth/register", "revocation_endpoint": b + "/oauth/revoke", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"}, "scopes_supported": []string{oauthScope}})
}
func (a *App) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	reply(w, 200, map[string]any{"resource": a.cfg.PublicURL + "/mcp", "authorization_servers": []string{a.cfg.PublicURL}, "scopes_supported": []string{oauthScope}, "bearer_methods_supported": []string{"header"}})
}
func (a *App) limit(w http.ResponseWriter, r *http.Request, key string, max int, ttl time.Duration) bool {
	n, e := a.store.Increment(r.Context(), "rate:"+key, ttl)
	if e != nil {
		failure(w, 503, "storage_unavailable")
		return false
	}
	if n > max {
		w.Header().Set("Retry-After", "60")
		failure(w, 429, "rate_limited")
		return false
	}
	return true
}
func (a *App) validRedirect(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" && u.Host == "claude.ai" && u.Path == "/api/mcp/auth_callback" && u.RawQuery == "" {
		return true
	}
	return a.cfg.AllowLocal && u.Scheme == "http" && loopback(u.Hostname()) && u.Host != ""
}
func (a *App) register(w http.ResponseWriter, r *http.Request) {
	if !a.limit(w, r, "register", 30, time.Hour) {
		return
	}
	var b map[string]json.RawMessage
	if !body(w, r, &b) {
		return
	}
	c := oauthClient{ID: random(), Method: "none"}
	if json.Unmarshal(b["redirect_uris"], &c.Redirects) != nil || len(c.Redirects) == 0 || len(c.Redirects) > 5 {
		failure(w, 400, "invalid_redirect_uri")
		return
	}
	for _, u := range c.Redirects {
		if len(u) > 2048 || !a.validRedirect(u) {
			failure(w, 400, "invalid_redirect_uri")
			return
		}
	}
	if v, ok := b["client_name"]; ok {
		if json.Unmarshal(v, &c.Name) != nil || len(c.Name) > 100 {
			failure(w, 400, "invalid_client_metadata")
			return
		}
	}
	if v, ok := b["token_endpoint_auth_method"]; ok {
		if json.Unmarshal(v, &c.Method) != nil {
			failure(w, 400, "invalid_client_metadata")
			return
		}
	}
	secret := ""
	switch c.Method {
	case "none":
	case "client_secret_post", "client_secret_basic":
		secret = random()
		c.SecretHash = hash(secret)
	default:
		failure(w, 400, "invalid_client_metadata")
		return
	}
	if e := a.save(r.Context(), "client:"+c.ID, c, 30*24*time.Hour); e != nil {
		failure(w, 503, "storage_unavailable")
		return
	}
	out := map[string]any{"client_id": c.ID, "client_name": c.Name, "redirect_uris": c.Redirects, "token_endpoint_auth_method": c.Method, "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "client_id_issued_at": time.Now().Unix()}
	if secret != "" {
		out["client_secret"] = secret
		out["client_secret_expires_at"] = time.Now().Add(30 * 24 * time.Hour).Unix()
	}
	reply(w, 201, out)
}
func (a *App) csrfName() string {
	if strings.HasPrefix(a.cfg.PublicURL, "https://") {
		return "__Host-bridge-csrf"
	}
	return "bridge-csrf"
}
func (a *App) cookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(a.cfg.PublicURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}
func (a *App) csrf(w http.ResponseWriter, r *http.Request) string {
	if c, e := r.Cookie(a.csrfName()); e == nil && len(c.Value) == 43 {
		return c.Value
	}
	v := random()
	a.cookie(w, a.csrfName(), v, 1800)
	return v
}
func (a *App) validCSRF(r *http.Request) bool {
	c, e := r.Cookie(a.csrfName())
	return e == nil && len(c.Value) == 43 && equal(c.Value, r.PostForm.Get("csrf"))
}
func parseForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	if r.ParseForm() != nil {
		failure(w, 400, "invalid_request")
		return false
	}
	return true
}
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
func (a *App) authorize(w http.ResponseWriter, r *http.Request) {
	if !a.limit(w, r, "authorize", 60, time.Minute) {
		return
	}
	q := r.URL.Query()
	var c oauthClient
	if a.load(r.Context(), "client:"+q.Get("client_id"), &c) != nil || !contains(c.Redirects, q.Get("redirect_uri")) {
		failure(w, 400, "invalid_client_or_redirect")
		return
	}
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || !pkcePattern.MatchString(q.Get("code_challenge")) || len(q.Get("code_challenge")) != 43 || len(q.Get("state")) > 1024 {
		failure(w, 400, "invalid_request")
		return
	}
	if s := q.Get("scope"); s != "" && s != oauthScope {
		failure(w, 400, "invalid_scope")
		return
	}
	if resource := q.Get("resource"); resource != "" && resource != a.cfg.PublicURL+"/mcp" {
		failure(w, 400, "invalid_target")
		return
	}
	csrf := a.csrf(w, r)
	id := random()
	p := authorization{ClientID: c.ID, Redirect: q.Get("redirect_uri"), Challenge: q.Get("code_challenge"), State: q.Get("state"), CSRFHash: hash(csrf)}
	if a.save(r.Context(), "consent:"+hash(id), p, 5*time.Minute) != nil {
		failure(w, 503, "storage_unavailable")
		return
	}
	a.page(w, "授权 Claude 访问测试目录", map[string]any{"Consent": id, "CSRF": csrf, "Client": c.Name, "Authorize": true})
}
func (a *App) approve(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) || !a.limit(w, r, "owner-login", 10, time.Minute) {
		return
	}
	if !a.validCSRF(r) {
		failure(w, 403, "invalid_csrf")
		return
	}
	if !equal(r.PostForm.Get("secret"), a.cfg.AdminSecret) {
		failure(w, 401, "invalid_owner_secret")
		return
	}
	id := r.PostForm.Get("consent")
	var p authorization
	if a.load(r.Context(), "consent:"+hash(id), &p) != nil || !equal(p.CSRFHash, hash(r.PostForm.Get("csrf"))) {
		failure(w, 400, "expired_consent")
		return
	}
	raw, e := a.store.Take(r.Context(), "consent:"+hash(id))
	if e != nil || raw == "" {
		failure(w, 400, "expired_consent")
		return
	}
	code := random()
	if a.save(r.Context(), "code:"+hash(code), p, 2*time.Minute) != nil {
		failure(w, 503, "storage_unavailable")
		return
	}
	u, _ := url.Parse(p.Redirect)
	q := u.Query()
	q.Set("code", code)
	if p.State != "" {
		q.Set("state", p.State)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}
func (a *App) clientAuth(r *http.Request) (oauthClient, bool) {
	id, secret := r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	basic := false
	if i, s, ok := r.BasicAuth(); ok {
		if id != "" && id != i {
			return oauthClient{}, false
		}
		id, secret, basic = i, s, true
	}
	var c oauthClient
	if a.load(r.Context(), "client:"+id, &c) != nil {
		return c, false
	}
	switch c.Method {
	case "none":
		return c, !basic && secret == ""
	case "client_secret_post":
		return c, !basic && equal(hash(secret), c.SecretHash)
	case "client_secret_basic":
		return c, basic && equal(hash(secret), c.SecretHash)
	}
	return c, false
}
func (a *App) token(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) || !a.limit(w, r, "token", 120, time.Minute) {
		return
	}
	c, ok := a.clientAuth(r)
	if !ok {
		failure(w, 401, "invalid_client")
		return
	}
	resource := a.cfg.PublicURL + "/mcp"
	if v := r.PostForm.Get("resource"); v != "" && v != resource {
		failure(w, 400, "invalid_target")
		return
	}
	g := accessGrant{ClientID: c.ID, Resource: resource, Scope: oauthScope}
	var consumeKey string
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		consumeKey = "code:" + hash(r.PostForm.Get("code"))
		var p authorization
		if a.load(r.Context(), consumeKey, &p) != nil || p.ClientID != c.ID || p.Redirect != r.PostForm.Get("redirect_uri") {
			failure(w, 400, "invalid_grant")
			return
		}
		verifier := r.PostForm.Get("code_verifier")
		if len(verifier) < 43 || len(verifier) > 128 {
			failure(w, 400, "invalid_grant")
			return
		}
		v := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(v[:])
		if !equal(challenge, p.Challenge) {
			failure(w, 400, "invalid_grant")
			return
		}
		g.GrantID = random()
	case "refresh_token":
		consumeKey = "refresh:" + hash(r.PostForm.Get("refresh_token"))
		if a.load(r.Context(), consumeKey, &g) != nil || g.ClientID != c.ID || !a.active(r, g) {
			failure(w, 400, "invalid_grant")
			return
		}
		if s := r.PostForm.Get("scope"); s != "" && s != g.Scope {
			failure(w, 400, "invalid_scope")
			return
		}
	default:
		failure(w, 400, "unsupported_grant_type")
		return
	}
	raw, e := a.store.Take(r.Context(), consumeKey)
	if e != nil || raw == "" {
		failure(w, 400, "invalid_grant")
		return
	}
	if r.PostForm.Get("grant_type") == "authorization_code" {
		epoch, e := a.store.Get(r.Context(), "epoch")
		if e != nil {
			failure(w, 503, "storage_unavailable")
			return
		}
		if epoch == "" {
			epoch = "0"
		}
		if e = a.store.Set(r.Context(), "grant:"+g.GrantID, epoch, grantTTL); e != nil {
			failure(w, 503, "storage_unavailable")
			return
		}
	}
	access, refresh := random(), random()
	if a.save(r.Context(), "access:"+hash(access), g, time.Hour) != nil || a.save(r.Context(), "refresh:"+hash(refresh), g, 7*24*time.Hour) != nil {
		failure(w, 503, "storage_unavailable")
		return
	}
	reply(w, 200, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600, "refresh_token": refresh, "scope": g.Scope})
}
func (a *App) active(r *http.Request, g accessGrant) bool {
	if g.Resource != a.cfg.PublicURL+"/mcp" || g.Scope != oauthScope {
		return false
	}
	epoch, e := a.store.Get(r.Context(), "epoch")
	if e != nil {
		return false
	}
	if epoch == "" {
		epoch = "0"
	}
	v, e := a.store.Get(r.Context(), "grant:"+g.GrantID)
	return e == nil && v != "" && v == epoch
}
func (a *App) protectedMCP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var g accessGrant
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || a.load(r.Context(), "access:"+hash(token), &g) != nil || !a.active(r, g) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+a.cfg.PublicURL+`/.well-known/oauth-protected-resource/mcp"`)
		failure(w, 401, "invalid_token")
		return
	}
	a.mcp.ServeHTTP(w, r)
}
func (a *App) revoke(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) || !a.limit(w, r, "revoke", 60, time.Minute) {
		return
	}
	c, ok := a.clientAuth(r)
	if !ok {
		failure(w, 401, "invalid_client")
		return
	}
	for _, prefix := range []string{"access:", "refresh:"} {
		var g accessGrant
		if a.load(r.Context(), prefix+hash(r.PostForm.Get("token")), &g) == nil && g.ClientID == c.ID {
			if _, e := a.store.Take(r.Context(), "grant:"+g.GrantID); e != nil {
				failure(w, 503, "storage_unavailable")
				return
			}
		}
	}
	w.WriteHeader(200)
}
