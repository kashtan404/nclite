package server

import (
	"fmt"
	"html"
	"net/http"
	"strings"
	"sync"
	"time"

	"nclite/internal/store"
)

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"installed": true, "maintenance": false, "needsDbUpgrade": false,
		"version": ncVersionFull, "versionstring": ncVersionString,
		"edition": "", "productname": s.cfg.ProductName, "extendedSupport": false,
	})
}

// ocsReply writes the OCS envelope. v1 reports status 100, v2 reports the HTTP status.
func ocsReply(w http.ResponseWriter, r *http.Request, httpCode int, data any) {
	v1 := strings.HasPrefix(r.URL.Path, "/ocs/v1.php")
	code, status := httpCode, "ok"
	if httpCode >= 400 {
		status = "failure"
	} else if v1 {
		code = 100
	}
	if v1 && httpCode >= 400 {
		code = 997
	}
	if data == nil {
		data = []any{}
	}
	hc := httpCode
	if v1 && httpCode < 400 {
		hc = http.StatusOK
	}
	writeJSON(w, hc, map[string]any{"ocs": map[string]any{
		"meta": map[string]any{"status": status, "statuscode": code, "message": http.StatusText(httpCode), "totalitems": "", "itemsperpage": ""},
		"data": data,
	}})
}

func (s *Server) handleOCS(w http.ResponseWriter, r *http.Request) {
	sub := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/ocs/v2.php"), "/ocs/v1.php")
	switch {
	case sub == "/cloud/capabilities":
		s.ocsCapabilities(w, r)
	case sub == "/cloud/user":
		s.ocsUser(w, r, "")
	case strings.HasPrefix(sub, "/cloud/users/") && !strings.Contains(strings.TrimPrefix(sub, "/cloud/users/"), "/"):
		// The iOS app asks for its own profile here before it fetches capabilities.
		// If this answers with an empty reply the app silently skips capabilities,
		// avatar and quota refresh (no item menu, quota stuck at "0 KB").
		s.ocsUser(w, r, strings.TrimPrefix(sub, "/cloud/users/"))
	case sub == "/apps/dav/api/v1/direct":
		s.ocsDirect(w, r)
	case sub == "/core/apppassword" && r.Method == http.MethodDelete:
		s.ocsDeleteAppPassword(w, r)
	default:
		// notifications, activity, user_status, dashboard, shares, ... are not
		// implemented: answer with a valid empty OCS reply so the client shrugs.
		ocsReply(w, r, http.StatusOK, nil)
	}
}

func (s *Server) ocsCapabilities(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	ocsReply(w, r, http.StatusOK, map[string]any{
		"version": map[string]any{"major": 28, "minor": 0, "micro": 4, "string": ncVersionString, "edition": "", "extendedSupport": false},
		"capabilities": map[string]any{
			"core":  map[string]any{"pollinterval": 60, "webdav-root": "remote.php/webdav", "reference-api": false},
			"files": map[string]any{"bigfilechunking": true, "blacklisted_files": []string{".htaccess"}, "undelete": true, "versioning": false, "comments": false},
			"dav":   map[string]any{"chunking": "1.0"},
			"files_sharing": map[string]any{
				"api_enabled": false, "resharing": false, "group_sharing": false,
				"public": map[string]any{"enabled": false},
			},
			"theming": map[string]any{
				"name": s.cfg.ProductName, "url": base, "slogan": "", "color": "#0082c9", "color-text": "#ffffff",
				"color-element": "#0082c9", "color-element-bright": "#0082c9", "color-element-dark": "#0082c9",
				"logo": "", "background": "", "background-plain": false, "background-default": true,
				"logoheader": "", "favicon": "",
			},
		},
	})
}

// ocsUser answers /cloud/user (want == "") and /cloud/users/<id>. As on a real
// server, a normal user can only read their own profile.
func (s *Server) ocsUser(w http.ResponseWriter, r *http.Request, want string) {
	name, ok := s.authed(w, r)
	if !ok {
		return
	}
	if want != "" && want != name {
		ocsReply(w, r, http.StatusForbidden, nil)
		return
	}
	u, _ := s.st.GetUser(name)
	if u == nil {
		ocsReply(w, r, http.StatusNotFound, nil)
		return
	}
	used := s.st.DirSize(name, "")
	free := freeSpace(s.cfg.DataDir)
	total := used + free
	rel := 0.0
	if total > 0 {
		rel = float64(used) / float64(total) * 100
	}
	ocsReply(w, r, http.StatusOK, map[string]any{
		"enabled": true, "storageLocation": s.fs.FilesRoot(name), "id": name, "lastLogin": 0,
		"backend": "Database", "subadmin": []string{},
		"quota": map[string]any{"free": free, "used": used, "total": total, "relative": rel, "quota": -3},
		"email": nil, "displayname": u.Display, "display-name": u.Display,
		"phone": "", "address": "", "website": "", "twitter": "", "groups": []string{},
		"language": "en", "locale": "", "notify_email": nil,
	})
}

func (s *Server) ocsDeleteAppPassword(w http.ResponseWriter, r *http.Request) {
	name, ok := s.authed(w, r)
	if !ok {
		return
	}
	if _, pw, _ := r.BasicAuth(); pw != "" && s.st.UserByAppHash(sha256hex(pw)) == name {
		s.st.DeleteAppPassword(sha256hex(pw))
	}
	ocsReply(w, r, http.StatusOK, nil)
}

// ---- Login Flow v2 -------------------------------------------------------------

type flow struct {
	pollToken, loginToken string
	created               time.Time
	user, appPassword     string
}

type flowStore struct {
	mu      sync.Mutex
	byPoll  map[string]*flow
	byLogin map[string]*flow
}

func newFlowStore() *flowStore {
	return &flowStore{byPoll: map[string]*flow{}, byLogin: map[string]*flow{}}
}

const flowTTL = 20 * time.Minute

func (f *flowStore) gc() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, fl := range f.byPoll {
		if time.Since(fl.created) > flowTTL {
			delete(f.byPoll, k)
			delete(f.byLogin, fl.loginToken)
		}
	}
}

func (s *Server) handleLoginV2Start(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fl := &flow{pollToken: store.RandHex(32), loginToken: store.RandHex(32), created: time.Now()}
	s.flows.mu.Lock()
	if len(s.flows.byPoll) > 200 {
		s.flows.mu.Unlock()
		s.flows.gc()
		s.flows.mu.Lock()
	}
	s.flows.byPoll[fl.pollToken] = fl
	s.flows.byLogin[fl.loginToken] = fl
	s.flows.mu.Unlock()

	base := s.baseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"poll":  map[string]any{"token": fl.pollToken, "endpoint": base + "/index.php/login/v2/poll"},
		"login": base + "/index.php/login/v2/flow/" + fl.loginToken,
	})
}

func (s *Server) handleLoginV2Poll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tok := r.FormValue("token")
	s.flows.mu.Lock()
	fl := s.flows.byPoll[tok]
	if fl != nil && time.Since(fl.created) > flowTTL {
		delete(s.flows.byPoll, tok)
		delete(s.flows.byLogin, fl.loginToken)
		fl = nil
	}
	var done bool
	if fl != nil && fl.appPassword != "" {
		done = true
		delete(s.flows.byPoll, tok)
		delete(s.flows.byLogin, fl.loginToken)
	}
	s.flows.mu.Unlock()
	if !done {
		http.NotFound(w, r) // Nextcloud answers 404 while the user has not finished logging in
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server": s.baseURL(r), "loginName": fl.user, "appPassword": fl.appPassword,
	})
}

const flowPage = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s</title><style>
body{font:16px system-ui,sans-serif;margin:0;display:grid;place-items:center;min-height:100vh;background:#f4f6f8;color:#222}
form,.box{background:#fff;padding:28px;border-radius:12px;box-shadow:0 2px 12px #0002;width:min(340px,88vw)}
input{display:block;width:100%%;box-sizing:border-box;margin:10px 0;padding:11px;font-size:16px;border:1px solid #bbb;border-radius:8px}
button{width:100%%;padding:12px;font-size:16px;border:0;border-radius:8px;background:#0082c9;color:#fff}
.err{color:#b00020}@media(prefers-color-scheme:dark){body{background:#181a1c;color:#eee}form,.box{background:#26292c}input{background:#1d1f21;color:#eee;border-color:#555}}
</style></head><body>%s</body></html>`

func (s *Server) handleLoginV2Flow(w http.ResponseWriter, r *http.Request) {
	tok := strings.Trim(strings.TrimPrefix(r.URL.Path, "/index.php/login/v2/flow/"), "/")
	tok = strings.TrimPrefix(tok, "login/v2/flow/")
	s.flows.mu.Lock()
	fl := s.flows.byLogin[tok]
	s.flows.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	page := func(body string) { w.Write([]byte(fmt.Sprintf(flowPage, html.EscapeString(s.cfg.ProductName), body))) }
	if fl == nil || time.Since(fl.created) > flowTTL {
		w.WriteHeader(http.StatusNotFound)
		page(`<div class="box">This login link has expired. Start again from the app.</div>`)
		return
	}
	form := func(msg string) string {
		return `<form method="post"><h3>` + html.EscapeString(s.cfg.ProductName) + `</h3>` + msg +
			`<input name="user" placeholder="Username" autocomplete="username" autocapitalize="none" required>` +
			`<input name="password" type="password" placeholder="Password" autocomplete="current-password" required>` +
			`<button>Log in</button></form>`
	}
	if r.Method != http.MethodPost {
		page(form(""))
		return
	}
	ok, err := s.au.CheckPassword(r.FormValue("user"), r.FormValue("password"), s.clientIP(r))
	if err != nil {
		w.WriteHeader(http.StatusTooManyRequests)
		page(form(`<p class="err">Too many attempts. Try again later.</p>`))
		return
	}
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		page(form(`<p class="err">Wrong username or password.</p>`))
		return
	}
	plain, hash := newAppPassword()
	label := r.UserAgent()
	if len(label) > 120 {
		label = label[:120]
	}
	if err := s.st.AddAppPassword(r.FormValue("user"), hash, label); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.flows.mu.Lock()
	fl.user, fl.appPassword = r.FormValue("user"), plain
	s.flows.mu.Unlock()
	page(`<div class="box"><h3>Account connected</h3><p>You can close this window and return to the app.</p></div>`)
}
