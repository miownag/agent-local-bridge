package server

import (
	"fmt"
	"github.com/miownag/agent-local-bridge/internal/bridge"
	"html/template"
	"net/http"
	"sort"
	"time"
)

var view = template.Must(template.New("page").Parse(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>{{.Title}}</title><style>body{font:16px system-ui;max-width:760px;margin:70px auto;padding:24px;background:#f5f3ee;color:#24342c}h1{font-size:32px}section{background:white;border:1px solid #ddd;padding:24px;border-radius:12px;margin:20px 0}label{display:block;margin:14px 0}input{padding:10px;width:90%;font:inherit}button{background:#254c3c;color:white;border:0;padding:12px 18px;border-radius:6px;margin:8px 0;cursor:pointer}code{overflow-wrap:anywhere}table{width:100%;text-align:left;font-size:14px}td,th{padding:8px;border-bottom:1px solid #ddd}.muted{color:#65736b}</style><h1>{{.Title}}</h1>
{{if .Authorize}}<section><p>客户端：{{.Client}}</p><p>允许读取、创建和修改本地文件、搜索代码并执行 Bash 命令。支持绝对路径、隐藏目录和符号链接，没有目录沙箱。权限等同于启动 worker 的系统用户，命令可访问其环境变量与网络。</p><form action="/oauth/approve" method="post"><input type="hidden" name="consent" value="{{.Consent}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><label>所有者密钥<input name="secret" type="password" autocomplete="off" required></label><button>授权此客户端</button></form></section>
{{else}}{{if .Authenticated}}<section><h2>本地 worker</h2>{{if .Status.Online}}<p>在线 · 目录：<code>{{.Status.RootName}}</code></p>{{else}}<p>离线或已暂停</p>{{end}}<p>Claude 连接器地址：<code>{{.MCPURL}}</code></p><p class="muted">状态最多延迟 30 秒。最近记录不包含路径、文件内容或凭证。</p></section><section><h2>连接管理</h2><form action="/admin/revoke" method="post"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>撤销所有 Claude 授权</button></form><form action="/admin/pause" method="post"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>暂停桥接</button></form><form action="/admin/resume" method="post"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>恢复桥接</button></form><p class="muted">暂停或撤销不能撤回已经执行的操作；停止本地 worker 会终止其 Bash 会话。</p></section><section><h2>最近调用</h2><table><tr><th>时间 UTC</th><th>工具</th><th>状态</th></tr>{{range .Events}}<tr><td>{{.At}}</td><td>{{.Tool}}</td><td>{{.State}}</td></tr>{{else}}<tr><td colspan="3">暂无调用</td></tr>{{end}}</table></section>
{{else}}<section><p>连接 Claude 云端与本地电脑。</p><form action="/admin/login" method="post"><input type="hidden" name="csrf" value="{{.CSRF}}"><label>所有者密钥<input type="password" name="secret" autocomplete="off" required></label><button>查看连接状态</button></form></section>{{end}}{{end}}<p class="muted">agent-local-bridge · 本地 coding agent</p></html>`))

func (a *App) page(w http.ResponseWriter, title string, data map[string]any) {
	data["Title"] = title
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = view.Execute(w, data)
}
func (a *App) sessionName() string {
	if a.cfg.AllowLocal {
		return "bridge-owner"
	}
	return "__Host-bridge-owner"
}
func (a *App) owner(r *http.Request) bool {
	c, e := r.Cookie(a.sessionName())
	if e != nil {
		return false
	}
	v, e := a.store.Get(r.Context(), "owner:"+hash(c.Value))
	return e == nil && v == "1"
}
func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"CSRF": a.csrf(w, r), "Authenticated": a.owner(r), "MCPURL": a.cfg.PublicURL + "/mcp"}
	if data["Authenticated"] == true {
		s, e := a.status(r.Context())
		if e != nil {
			failure(w, 503, "storage_unavailable")
			return
		}
		data["Status"] = s
		events := []bridge.Event{}
		for i := 0; i < 20; i++ {
			var b bridge.Event
			if a.load(r.Context(), fmt.Sprintf("event:%d", i), &b) == nil {
				events = append(events, b)
			}
		}
		sort.Slice(events, func(i, j int) bool { return events[i].At > events[j].At })
		data["Events"] = events
	}
	a.page(w, "Agent Local Bridge", data)
}
func (a *App) adminLogin(w http.ResponseWriter, r *http.Request) {
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
	t := random()
	if a.store.Set(r.Context(), "owner:"+hash(t), "1", 30*time.Minute) != nil {
		failure(w, 503, "storage_unavailable")
		return
	}
	a.cookie(w, a.sessionName(), t, 1800)
	http.Redirect(w, r, "/", 303)
}
func (a *App) adminCheck(w http.ResponseWriter, r *http.Request) bool {
	if !parseForm(w, r) {
		return false
	}
	if !a.owner(r) || !a.validCSRF(r) {
		failure(w, 403, "owner_required")
		return false
	}
	return true
}
func (a *App) adminRevoke(w http.ResponseWriter, r *http.Request) {
	if !a.adminCheck(w, r) {
		return
	}
	if _, e := a.store.Increment(r.Context(), "epoch", 90*24*time.Hour); e != nil {
		failure(w, 503, "storage_unavailable")
		return
	}
	http.Redirect(w, r, "/", 303)
}
func (a *App) adminPause(w http.ResponseWriter, r *http.Request) {
	if !a.adminCheck(w, r) {
		return
	}
	if a.store.Set(r.Context(), "paused", "1", 365*24*time.Hour) != nil {
		failure(w, 503, "storage_unavailable")
		return
	}
	http.Redirect(w, r, "/", 303)
}
func (a *App) adminResume(w http.ResponseWriter, r *http.Request) {
	if !a.adminCheck(w, r) {
		return
	}
	if a.store.DeleteIf(r.Context(), "paused", "1") != nil {
		failure(w, 503, "storage_unavailable")
		return
	}
	http.Redirect(w, r, "/", 303)
}
