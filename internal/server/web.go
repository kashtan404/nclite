package server

import (
	"crypto/hmac"
	"embed"
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"nclite/internal/store"
	"nclite/internal/vfs"
)

// The file browser: a small web UI for looking at your own files, shared folders and
// albums from a computer. It is served only by the admin listener (admin_listen), under
// web_path (default /cloud), so it is as private as the admin pages.

//go:embed static/*
var staticFS embed.FS

type webPage struct {
	Base, Product, Error string
	LoggedIn             bool
}

// ---- session -----------------------------------------------------------------

const webCookie = "nclite_web"

func (s *Server) setWebSession(w http.ResponseWriter, r *http.Request, user string) {
	exp := strconv.FormatInt(time.Now().Add(12*time.Hour).Unix(), 10)
	v := user + "|" + exp
	http.SetCookie(w, &http.Cookie{Name: webCookie, Value: v + "|" + s.mac("web|"+v), Path: s.webPrefix,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.adminSecure(r), MaxAge: 12 * 3600})
}

// webSession returns the logged-in user and the raw cookie value, or "".
func (s *Server) webSession(r *http.Request) (string, string) {
	c, err := r.Cookie(webCookie)
	if err != nil {
		return "", ""
	}
	p := strings.Split(c.Value, "|")
	if len(p) != 3 || !hmac.Equal([]byte(p[2]), []byte(s.mac("web|"+p[0]+"|"+p[1]))) {
		return "", ""
	}
	if exp, _ := strconv.ParseInt(p[1], 10, 64); time.Now().Unix() > exp {
		return "", ""
	}
	if u, _ := s.st.GetUser(p[0]); u == nil {
		return "", ""
	}
	return p[0], c.Value
}

func (s *Server) registerWeb(m *http.ServeMux) {
	m.HandleFunc(s.webPrefix, s.handleWeb)
	m.HandleFunc(s.webPrefix+"/", s.handleWeb)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func (s *Server) handleWeb(w http.ResponseWriter, r *http.Request) {
	sub := strings.Trim(strings.TrimPrefix(r.URL.Path, s.webPrefix), "/")
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	switch {
	case sub == "":
		s.webPageHandler(w, r)
		return
	case sub == "login" && r.Method == http.MethodPost:
		s.webLogin(w, r)
		return
	case strings.HasPrefix(sub, "static/"):
		s.webStatic(w, r, strings.TrimPrefix(sub, "static/"))
		return
	}
	user, sess := s.webSession(r)
	if user == "" {
		jsonErr(w, http.StatusUnauthorized, "login required")
		return
	}
	if r.Method == http.MethodPost && !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.csrf(sess))) {
		jsonErr(w, http.StatusForbidden, "bad CSRF token")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	switch sub {
	case "logout":
		http.SetCookie(w, &http.Cookie{Name: webCookie, Path: s.webPrefix, MaxAge: -1})
		writeJSON(w, 200, map[string]any{"ok": true})
	case "api/me":
		writeJSON(w, 200, map[string]any{"user": user, "csrf": s.csrf(sess)})
	case "api/list":
		s.webAPIList(w, r, user)
	case "api/tree":
		s.webAPITree(w, r, user)
	case "api/albums":
		s.webAPIAlbums(w, r, user)
	case "api/album":
		s.webAPIAlbum(w, r, user)
	case "api/op":
		s.webAPIOp(w, r, user)
	case "api/albumop":
		s.webAPIAlbumOp(w, r, user)
	case "file":
		s.webFile(w, r, user)
	case "preview":
		s.webPreview(w, r, user)
	default:
		jsonErr(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) webPageHandler(w http.ResponseWriter, r *http.Request) {
	user, _ := s.webSession(r)
	// scripts and styles only from our own origin: nothing a file contains can inject code
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; "+
		"img-src 'self'; media-src 'self'; frame-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pg := webPage{Base: s.webPrefix, Product: s.cfg.ProductName, LoggedIn: user != "", Error: r.URL.Query().Get("e")}
	if err := adminTpl.ExecuteTemplate(w, "web.html", pg); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *Server) webLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	user := r.FormValue("user")
	ok, err := s.au.CheckPassword(user, r.FormValue("password"), s.clientIP(r))
	if err != nil {
		http.Redirect(w, r, s.webPrefix+"?e="+urlQ("Too many attempts, try later"), http.StatusSeeOther)
		return
	}
	if !ok {
		http.Redirect(w, r, s.webPrefix+"?e="+urlQ("Wrong user name or password"), http.StatusSeeOther)
		return
	}
	s.setWebSession(w, r, user)
	http.Redirect(w, r, s.webPrefix, http.StatusSeeOther)
}

func (s *Server) webStatic(w http.ResponseWriter, r *http.Request, name string) {
	data, err := staticFS.ReadFile("static/" + name)
	if err != nil || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	ct := "text/plain; charset=utf-8"
	switch path.Ext(name) {
	case ".js":
		ct = "text/javascript; charset=utf-8"
	case ".css":
		ct = "text/css; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

// ---- listings ----------------------------------------------------------------

type webEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"` // as the user sees it, e.g. "Photos (bob)/2024/a.jpg"
	Dir     bool   `json:"dir"`
	Size    int64  `json:"size"`
	Mtime   int64  `json:"mtime"`
	ID      int64  `json:"id"`
	Mime    string `json:"mime,omitempty"`
	Preview bool   `json:"preview,omitempty"`
	Text    bool   `json:"text,omitempty"` // shown as plain text in the viewer (decided here, not guessed by the page)
	RO      bool   `json:"ro,omitempty"`
	Fav     bool   `json:"fav,omitempty"`
	Owner   string `json:"owner,omitempty"`
}

func (s *Server) webEntryOf(n *store.Node, visible string, ro bool, favs map[int64]bool, owner string) webEntry {
	e := webEntry{Name: path.Base(visible), Path: visible, Dir: n.IsDir, Mtime: n.Mtime, ID: n.ID, RO: ro, Fav: favs[n.ID], Owner: owner}
	if n.IsDir {
		e.Size = s.st.DirSize(n.Owner, n.Path)
	} else {
		e.Size = n.Size
		e.Mime = mimeOf(e.Name)
		e.Preview = s.pv.Supported(e.Name)
		e.Text = textExt[strings.ToLower(path.Ext(e.Name))]
	}
	return e
}

func (s *Server) cleanQueryPath(r *http.Request, key string) (string, error) {
	return vfs.CleanRel(r.URL.Query().Get(key))
}

func (s *Server) webAPIList(w http.ResponseWriter, r *http.Request, user string) {
	rel, err := s.cleanQueryPath(r, "path")
	if err != nil {
		jsonErr(w, 400, "bad path")
		return
	}
	t := s.fs.Resolve(user, rel)
	t.Rel = s.fs.Canon(t.Owner, t.Rel)
	self, err := s.fs.NodeFor(t.Owner, t.Rel)
	if err != nil {
		code, msg := httpStatus(err)
		jsonErr(w, code, msg)
		return
	}
	if !self.IsDir {
		jsonErr(w, 400, "not a folder")
		return
	}
	kids, err := s.fs.ListDir(t.Owner, t.Rel)
	if err != nil {
		code, msg := httpStatus(err)
		jsonErr(w, code, msg)
		return
	}
	favs := s.st.FavoriteSet(user)
	out := make([]webEntry, 0, len(kids)+4)
	owner := ""
	if t.InMount {
		owner = t.Owner
	}
	for _, k := range kids {
		out = append(out, s.webEntryOf(k, store.Join(rel, path.Base(k.Path)), t.ReadOnly, favs, owner))
	}
	if rel == "" && !t.InMount {
		for _, m := range s.fs.Mounts(user) {
			if n, err := s.fs.NodeFor(m.Owner, m.Path); err == nil && n.IsDir {
				out = append(out, s.webEntryOf(n, m.Name, m.ReadOnly, favs, m.Owner))
			}
		}
	}
	writeJSON(w, 200, map[string]any{"path": rel, "readonly": t.ReadOnly, "entries": out})
}

// webAPITree lists only the sub-folders of a folder, straight from the index, so the
// tree in the sidebar stays instant even for folders with tens of thousands of files.
func (s *Server) webAPITree(w http.ResponseWriter, r *http.Request, user string) {
	rel, err := s.cleanQueryPath(r, "path")
	if err != nil {
		jsonErr(w, 400, "bad path")
		return
	}
	type item struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	dirs := []item{}
	t := s.fs.Resolve(user, rel)
	t.Rel = s.fs.Canon(t.Owner, t.Rel)
	if rel == "" && !t.InMount {
		if _, err := s.fs.ListDir(user, ""); err != nil { // makes sure the top level is indexed
			code, msg := httpStatus(err)
			jsonErr(w, code, msg)
			return
		}
	}
	if s.st.GetMeta("lastscan:"+t.Owner) == "" { // index not complete yet: read this folder from disk
		s.fs.ListDir(t.Owner, t.Rel)
	}
	nodes, _ := s.st.ChildDirs(t.Owner, t.Rel)
	for _, n := range nodes {
		name := path.Base(n.Path)
		dirs = append(dirs, item{name, store.Join(rel, name)})
	}
	if rel == "" && !t.InMount {
		for _, m := range s.fs.Mounts(user) {
			dirs = append(dirs, item{m.Name, m.Name})
		}
	}
	sort.SliceStable(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })
	writeJSON(w, 200, map[string]any{"dirs": dirs})
}

func (s *Server) webAPIAlbums(w http.ResponseWriter, r *http.Request, user string) {
	albums, err := s.st.ListAlbums(user)
	if err != nil {
		jsonErr(w, 500, "database error")
		return
	}
	type item struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Count int64  `json:"count"`
		Cover int64  `json:"cover"`
	}
	out := make([]item, 0, len(albums))
	for i := range albums {
		st := albumStatsOf(s.albumNodes(user, &albums[i]))
		out = append(out, item{albums[i].ID, albums[i].Name, st.count, st.last})
	}
	writeJSON(w, 200, map[string]any{"albums": out})
}

func (s *Server) webAPIAlbum(w http.ResponseWriter, r *http.Request, user string) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	a, err := s.st.AlbumByID(user, id)
	if err != nil || a == nil {
		jsonErr(w, 404, "no such album")
		return
	}
	favs := s.st.FavoriteSet(user)
	out := []webEntry{}
	for _, n := range s.albumNodes(user, a) {
		rel, ro, ok := s.fs.VisiblePath(user, n)
		if !ok {
			continue
		}
		owner := ""
		if n.Owner != user {
			owner = n.Owner
		}
		out = append(out, s.webEntryOf(n, rel, ro, favs, owner))
	}
	writeJSON(w, 200, map[string]any{"album": map[string]any{"id": a.ID, "name": a.Name}, "entries": out})
}

// ---- operations --------------------------------------------------------------

type opReq struct {
	Op    string   `json:"op"`
	Items []string `json:"items"`
	Dest  string   `json:"dest"`
	Name  string   `json:"name"`
	ID    int64    `json:"id"`
	IDs   []int64  `json:"ids"`
}

type opResult struct {
	Path  string `json:"path"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		jsonErr(w, 405, "POST only")
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		jsonErr(w, 400, "bad request")
		return false
	}
	return true
}

func friendly(err error) string {
	code, msg := httpStatus(err)
	switch code {
	case 403:
		if msg != "Forbidden" {
			return msg
		}
		return "not allowed"
	case 404:
		return "not found"
	case 409:
		return "destination folder does not exist"
	case 412:
		return "a file or folder with that name already exists there"
	case 503:
		return "storage unavailable"
	}
	return msg
}

func (s *Server) webAPIOp(w http.ResponseWriter, r *http.Request, user string) {
	var q opReq
	if !readJSON(w, r, &q) {
		return
	}
	var items []string
	for _, it := range q.Items {
		rel, err := vfs.CleanRel(it)
		if err != nil || rel == "" {
			jsonErr(w, 400, "bad path")
			return
		}
		items = append(items, rel)
	}
	results := []opResult{}
	add := func(p string, err error) {
		res := opResult{Path: p, OK: err == nil}
		if err != nil {
			res.Error = friendly(err)
		}
		results = append(results, res)
	}
	switch q.Op {
	case "copy", "move":
		dest, err := vfs.CleanRel(q.Dest)
		if err != nil {
			jsonErr(w, 400, "bad destination")
			return
		}
		for _, it := range items {
			_, _, err := s.doMoveCopy(user, it, store.Join(dest, path.Base(it)), q.Op == "move", false)
			add(it, err)
		}
	case "rename":
		if len(items) != 1 || !validName(q.Name) {
			jsonErr(w, 400, "give one item and a valid new name")
			return
		}
		_, _, err := s.doMoveCopy(user, items[0], store.Join(store.ParentOf(items[0]), q.Name), true, false)
		add(items[0], err)
	case "delete":
		for _, it := range items {
			add(it, s.doDelete(user, it))
		}
	case "mkdir":
		parent, err := vfs.CleanRel(q.Dest)
		if err != nil || !validName(q.Name) {
			jsonErr(w, 400, "bad folder name")
			return
		}
		_, err = s.doMkdir(user, store.Join(parent, q.Name))
		add(store.Join(parent, q.Name), err)
	default:
		jsonErr(w, 400, "unknown operation")
		return
	}
	ok := true
	for _, res := range results {
		ok = ok && res.OK
	}
	writeJSON(w, 200, map[string]any{"ok": ok, "results": results})
}

func (s *Server) webAPIAlbumOp(w http.ResponseWriter, r *http.Request, user string) {
	var q opReq
	if !readJSON(w, r, &q) {
		return
	}
	album := func() *store.Album {
		a, _ := s.st.AlbumByID(user, q.ID)
		if a == nil {
			jsonErr(w, 404, "no such album")
		}
		return a
	}
	switch q.Op {
	case "create":
		if !validAlbumName(q.Name) {
			jsonErr(w, 400, "invalid album name")
			return
		}
		a, err := s.st.CreateAlbum(user, q.Name)
		if err == store.ErrExists {
			jsonErr(w, 409, "an album with that name already exists")
			return
		} else if err != nil {
			jsonErr(w, 500, "database error")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "id": a.ID})
	case "rename":
		a := album()
		if a == nil {
			return
		}
		if !validAlbumName(q.Name) {
			jsonErr(w, 400, "invalid album name")
			return
		}
		if err := s.st.RenameAlbum(a.ID, q.Name); err == store.ErrExists {
			jsonErr(w, 409, "an album with that name already exists")
			return
		} else if err != nil {
			jsonErr(w, 500, "database error")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case "delete":
		a := album()
		if a == nil {
			return
		}
		s.st.DeleteAlbum(a.ID) // only the list; the files are not touched
		writeJSON(w, 200, map[string]any{"ok": true})
	case "add":
		a := album()
		if a == nil {
			return
		}
		added, skipped := 0, 0
		addNode := func(n *store.Node) {
			if n == nil || n.IsDir {
				skipped++
				return
			}
			if ok, err := s.st.AlbumAdd(a.ID, n.ID); err == nil && ok {
				added++
			}
		}
		for _, it := range q.Items {
			rel, err := vfs.CleanRel(it)
			if err != nil {
				skipped++
				continue
			}
			t := s.fs.Resolve(user, rel)
			n, err := s.fs.NodeFor(t.Owner, s.fs.Canon(t.Owner, t.Rel))
			if err != nil {
				skipped++
				continue
			}
			addNode(n)
		}
		for _, id := range q.IDs {
			if n, _ := s.st.NodeByID(id); n != nil {
				if _, ok := s.fs.CanAccess(user, n); ok {
					addNode(n)
					continue
				}
			}
			skipped++
		}
		writeJSON(w, 200, map[string]any{"ok": true, "added": added, "skipped": skipped})
	case "remove":
		a := album()
		if a == nil {
			return
		}
		for _, id := range q.IDs {
			s.st.AlbumRemove(a.ID, id)
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		jsonErr(w, 400, "unknown operation")
	}
}

// ---- file content and previews ----------------------------------------------------

// inlineSafe are the types a browser may display inside our page. Everything else
// (HTML, SVG, scripts, ...) is only ever offered as a download, so a file shared by
// someone else can never run as a page of this site with your session.
var textExt = map[string]bool{".txt": true, ".md": true, ".json": true, ".log": true, ".csv": true,
	".ini": true, ".conf": true, ".yaml": true, ".yml": true, ".xml": true, ".srt": true}

func inlineSafe(mt string) bool {
	switch mt {
	case "text/plain; charset=utf-8", "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp", "application/pdf":
		return true
	}
	return strings.HasPrefix(mt, "video/") || strings.HasPrefix(mt, "audio/")
}

func (s *Server) webFile(w http.ResponseWriter, r *http.Request, user string) {
	rel, err := s.cleanQueryPath(r, "path")
	if err != nil || rel == "" {
		jsonErr(w, 400, "bad path")
		return
	}
	t := s.fs.Resolve(user, rel)
	n, err := s.fs.NodeFor(t.Owner, s.fs.Canon(t.Owner, t.Rel))
	if err != nil {
		code, msg := httpStatus(err)
		jsonErr(w, code, msg)
		return
	}
	if n.IsDir {
		jsonErr(w, 400, "is a folder")
		return
	}
	f, err := os.Open(s.fs.Real(n.Owner, n.Path))
	if err != nil {
		jsonErr(w, 404, "not found")
		return
	}
	defer f.Close()
	name := path.Base(n.Path)
	mt := mimeOf(name)
	if textExt[strings.ToLower(path.Ext(name))] {
		mt = "text/plain; charset=utf-8" // shown as text, never interpreted
	}
	disp := "inline"
	if r.URL.Query().Get("dl") == "1" || !inlineSafe(mt) {
		disp = "attachment"
		if !inlineSafe(mt) {
			mt = "application/octet-stream"
		}
	}
	h := w.Header()
	h.Set("Content-Type", mt)
	h.Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": name}))
	h.Set("Cache-Control", "private, no-cache")
	h.Set("ETag", `"`+n.ETag+`"`)
	http.ServeContent(w, r, name, time.Unix(n.Mtime, 0), f)
}

func (s *Server) webPreview(w http.ResponseWriter, r *http.Request, user string) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	n, err := s.st.NodeByID(id)
	if err != nil || n == nil || n.IsDir {
		http.NotFound(w, r)
		return
	}
	if _, ok := s.fs.CanAccess(user, n); !ok {
		http.NotFound(w, r)
		return
	}
	fresh, err := s.fs.NodeFor(n.Owner, n.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	size := "512"
	switch v := r.URL.Query().Get("s"); v {
	case "256", "512", "1024", "2048":
		size = v
	}
	q := url.Values{"x": {size}, "y": {size}, "a": {"1"}}
	r.URL.RawQuery = q.Encode()
	w.Header().Set("Cache-Control", "private, max-age=3600")
	s.servePreviewN(w, r, fresh, path.Base(fresh.Path), s.fs.Real(fresh.Owner, fresh.Path), false)
}
