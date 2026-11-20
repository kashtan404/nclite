// Package server implements the subset of the Nextcloud server API that the
// Nextcloud iOS client needs: status, OCS capabilities/user, Login Flow v2,
// WebDAV files, chunked uploads v2, previews, plus a small admin UI.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nclite/internal/config"
	"nclite/internal/preview"
	"nclite/internal/store"
	"nclite/internal/vfs"
)

// Version we claim to be. Recent enough that the iOS client enables all the
// features we implement (chunking v2, preview by file id).
const (
	ncVersionString = "28.0.4"
	ncVersionFull   = "28.0.4.1"
)

type Server struct {
	cfg        *config.Config
	st         *store.Store
	fs         *vfs.VFS
	pv         *preview.Manager
	au         *Authenticator
	instanceID string
	secret     []byte
	flows      *flowStore
	mux        *http.ServeMux

	adminPrefix string // URL path of the admin UI
	webPrefix   string // URL path of the file browser (served on the admin listener only)

	jobMu sync.Mutex
	job   *Job // the running or most recently finished admin job

	sizeMu  sync.Mutex
	sizeReq map[sizeSpec]int64 // preview sizes the apps asked for
}

func New(cfg *config.Config, st *store.Store, v *vfs.VFS, pv *preview.Manager) (*Server, error) {
	inst, err := st.Meta("instance_id", func() string { return "oc" + store.RandHex(5) })
	if err != nil {
		return nil, err
	}
	sec, err := st.Meta("session_secret", func() string { return store.RandHex(32) })
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, st: st, fs: v, pv: pv,
		au:         NewAuthenticator(st),
		instanceID: inst,
		secret:     []byte(sec),
		flows:      newFlowStore(),
		mux:        http.NewServeMux(),
	}
	s.adminPrefix = cfg.AdminPrefix()
	s.webPrefix = cfg.WebPrefix()
	if st.GetMeta("index_fmt") != "3" {
		// Older versions could index a folder a second time: under another letter case
		// on case-insensitive disks, and under "Photos/" when a shared folder was opened
		// by its recipient. Drop the malformed rows now and mark every account "never
		// scanned"; the startup scan then removes the remaining spellings that are not on disk.
		if n, err := st.PurgeBadPaths(); err == nil && n > 0 {
			slog.Info("removed duplicate index rows left by an older version", "rows", n)
		}
		if users, err := st.ListUsers(); err == nil {
			for _, u := range users {
				st.SetMeta("lastscan:"+u.Name, "")
			}
		}
		st.SetMeta("index_fmt", "3")
	}
	s.routes()
	return s, nil
}

func (s *Server) Handler() http.Handler {
	if s.cfg.AccessLog {
		return accessLog(s.mux)
	}
	return s.mux
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("/status.php", s.handleStatus)
	m.HandleFunc("/ocs/", s.handleOCS)

	for _, pre := range []string{"/index.php", ""} {
		m.HandleFunc(pre+"/login/v2", s.handleLoginV2Start)
		m.HandleFunc(pre+"/login/v2/poll", s.handleLoginV2Poll)
		m.HandleFunc(pre+"/login/v2/flow/", s.handleLoginV2Flow)
		m.HandleFunc(pre+"/core/preview", s.handlePreview)
		m.HandleFunc(pre+"/core/preview.png", s.handlePreview)
		m.HandleFunc(pre+"/apps/files_trashbin/preview", s.handleTrashPreview)
	}

	m.HandleFunc("/remote.php/dav/files/", s.handleDAV(true))
	m.HandleFunc("/remote.php/webdav/", s.handleDAV(false))
	m.HandleFunc("/remote.php/webdav", s.handleDAV(false))
	m.HandleFunc("/remote.php/dav/uploads/", s.handleUploads)
	m.HandleFunc("/remote.php/dav/trashbin/", s.handleTrash)
	m.HandleFunc("/remote.php/dav/photos/", s.handlePhotos)
	m.HandleFunc("/remote.php/direct/", s.handleDirect)
	m.HandleFunc("/remote.php/dav/", s.handleDAVRoot)
	m.HandleFunc("/remote.php/dav", s.handleDAVRoot)

	// With admin_listen set, the admin UI lives only on its own listener and the
	// public one answers 404 for it: the safe way to keep it off the internet.
	if s.cfg.AdminListen == "" {
		s.registerAdmin(m)
	}

	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && s.cfg.AdminListen == "" {
			http.Redirect(w, r, s.adminPrefix, http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})
}

func (s *Server) registerAdmin(m *http.ServeMux) {
	m.HandleFunc(s.adminPrefix, s.handleAdmin)
	m.HandleFunc(s.adminPrefix+"/", s.handleAdmin)
}

// AdminHandler serves the admin UI and the file browser, for the dedicated admin_listen
// listener. None of the file, OCS or login endpoints of the Nextcloud API exist there.
func (s *Server) AdminHandler() http.Handler {
	m := http.NewServeMux()
	s.registerAdmin(m)
	s.registerWeb(m)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">`+
			`<title>%[1]s</title><style>%[2]s</style><main style="max-width:420px;margin-top:60px"><section><h1>%[1]s</h1>`+
			`<p><a href="%[3]s/">Files, photos and albums</a></p><p><a href="%[4]s/">Administration</a></p></section></main>`,
			template.HTMLEscapeString(s.cfg.ProductName), sharedCSS(), s.webPrefix, s.adminPrefix)
	})
	if s.cfg.AccessLog {
		return accessLog(m)
	}
	return m
}

// ---- helpers -----------------------------------------------------------------

func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return s.cfg.BaseURL
	}
	scheme, host := "http", r.Host
	if r.TLS != nil {
		scheme = "https"
	}
	if s.cfg.TrustProxy {
		if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
			scheme = v
		}
		if v := r.Header.Get("X-Forwarded-Host"); v != "" {
			host = v
		}
	}
	return scheme + "://" + host
}

// fileIDString is Nextcloud's oc:id format: zero-padded id + instance id.
func (s *Server) fileIDString(id int64) string { return fmt.Sprintf("%08d%s", id, s.instanceID) }

var extraMime = map[string]string{
	".heic": "image/heic", ".heif": "image/heif", ".mov": "video/quicktime", ".mp4": "video/mp4",
	".m4v": "video/x-m4v", ".webp": "image/webp", ".pdf": "application/pdf", ".jpg": "image/jpeg",
	".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif", ".txt": "text/plain",
	".md": "text/markdown", ".json": "application/json", ".zip": "application/zip",
}

func mimeOf(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if m, ok := extraMime[ext]; ok {
		return m
	}
	if m := mime.TypeByExtension(ext); m != "" {
		mt, _, _ := strings.Cut(m, ";")
		return mt
	}
	return "application/octet-stream"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 256*1024); return &b }}

// copyBuf copies with a pooled 256 KiB buffer.
func copyBuf(dst io.Writer, src io.Reader) (int64, error) {
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	// hide ReaderFrom/WriterTo so the pooled buffer is really used
	return io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, *bp)
}

// ---- access log ----------------------------------------------------------------

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int)           { w.code = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r) // keep sendfile for downloads
	}
	return io.Copy(struct{ io.Writer }{w.ResponseWriter}, r)
}

func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		t := time.Now()
		next.ServeHTTP(sw, r)
		slog.Info("req", "m", r.Method, "path", r.URL.RequestURI(), "status", sw.code,
			"ms", time.Since(t).Milliseconds(), "ua", r.UserAgent())
	})
}

// ---- background maintenance ------------------------------------------------------

// Maintain runs periodic housekeeping until ctx is cancelled.
func (s *Server) Maintain(ctx context.Context) {
	hourly := time.NewTicker(time.Hour)
	defer hourly.Stop()
	var rescan <-chan time.Time
	if m := s.cfg.RescanMinutes; m > 0 {
		t := time.NewTicker(time.Duration(m) * time.Minute)
		defer t.Stop()
		rescan = t.C
	}
	go func() { // shortly after start: index accounts that were never scanned (e.g. an adopted data folder)
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
			s.fs.Rescan(ctx, true)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hourly.C:
			s.cleanChunks()
			s.flows.gc()
			s.st.GCDirect()
			s.purgeTrash()
			s.pv.GC(s.st.NodeExists)
		case <-rescan:
			s.fs.Rescan(ctx, false)
		}
	}
}

// cleanChunks removes abandoned chunked uploads older than 24 hours.
func (s *Server) cleanChunks() {
	users, err := s.st.ListUsers()
	if err != nil {
		return
	}
	for _, u := range users {
		base := filepath.Join(s.fs.TmpDir(u.Name), "chunks")
		ents, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
				os.RemoveAll(filepath.Join(base, e.Name()))
			}
		}
	}
}

func logPreviewErr(name string, err error) { slog.Warn("preview failed", "file", name, "err", err) }
