package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"nclite/internal/vfs"
)

//go:embed templates/*.html
var tplFS embed.FS

var (
	adminTpl = template.Must(template.New("").Funcs(template.FuncMap{"human": human}).ParseFS(tplFS, "templates/*.html"))
	reUser   = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)
)

func human(n int64) string {
	const u = "KMGT"
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	f, i := float64(n)/1024, 0
	for f >= 1024 && i < len(u)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %ciB", f, u[i])
}

type userView struct {
	Name       string
	Admin      bool
	Path       string
	Files      int64
	Dirs       int64
	Used       int64
	TrashItems int64
	TrashSize  int64
	LastScan   string
	Devices    int
}
type shareView struct {
	ID                  int64
	Owner, Path, Target string
	ReadOnly            bool
}
type adminPage struct {
	Base                            string
	Product, Flash, CSRF, Me        string
	LoggedIn                        bool
	Users                           []userView
	Shares                          []shareView
	Unclaimed                       []string
	Job                             *JobView
	Refresh                         bool
	DataDir, DBPath, CacheDir, Free string
	ReqSizes                        []string
	DefaultSizes                    string
	CaseInsensitive                 bool
}

// ---- session: signed cookie "user|expiry|mac" ---------------------------------

func (s *Server) mac(msg string) string {
	h := hmac.New(sha256.New, s.secret)
	h.Write([]byte(msg))
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, user string) {
	exp := strconv.FormatInt(time.Now().Add(12*time.Hour).Unix(), 10)
	v := user + "|" + exp
	http.SetCookie(w, &http.Cookie{Name: "nclite_admin", Value: v + "|" + s.mac("sess|"+v), Path: s.adminPrefix,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.adminSecure(r),
		MaxAge: 12 * 3600})
}

// session returns the logged-in admin and the raw cookie value, or "".
func (s *Server) session(r *http.Request) (string, string) {
	c, err := r.Cookie("nclite_admin")
	if err != nil {
		return "", ""
	}
	p := strings.Split(c.Value, "|")
	if len(p) != 3 || !hmac.Equal([]byte(p[2]), []byte(s.mac("sess|"+p[0]+"|"+p[1]))) {
		return "", ""
	}
	if exp, _ := strconv.ParseInt(p[1], 10, 64); time.Now().Unix() > exp {
		return "", ""
	}
	if u, _ := s.st.GetUser(p[0]); u == nil || !u.Admin {
		return "", ""
	}
	return p[0], c.Value
}

func (s *Server) csrf(sess string) string { return s.mac("csrf|" + sess)[:32] }

// ---- handlers -------------------------------------------------------------------

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	me, sess := s.session(r)

	if r.Method == http.MethodPost {
		s.adminPost(w, r, me, sess)
		return
	}
	if r.URL.Path != s.adminPrefix && r.URL.Path != s.adminPrefix+"/" {
		http.NotFound(w, r)
		return
	}
	s.renderAdmin(w, me, sess, r.URL.Query().Get("m"))
}

func (s *Server) renderAdmin(w http.ResponseWriter, me, sess, flash string) {
	pg := adminPage{Base: s.adminPrefix, Product: s.cfg.ProductName, Flash: flash, LoggedIn: me != "", Me: me}
	if me != "" {
		pg.CSRF = s.csrf(sess)
		pg.DataDir, pg.DBPath, pg.CacheDir = s.cfg.DataDir, s.cfg.DBPath, s.cfg.CacheDir
		pg.Free = human(freeSpace(s.cfg.DataDir))
		pg.CaseInsensitive = s.fs.CaseInsensitive()
		users, _ := s.st.ListUsers()
		known := map[string]bool{}
		for _, u := range users {
			known[u.Name] = true
			files, dirs, bytes := s.st.UserStats(u.Name)
			ti, tb := s.st.TrashStats(u.Name)
			last := "not scanned yet"
			if ts, err := strconv.ParseInt(s.st.GetMeta("lastscan:"+u.Name), 10, 64); err == nil {
				last = time.Unix(ts, 0).Format("2006-01-02 15:04")
			}
			pg.Users = append(pg.Users, userView{Name: u.Name, Admin: u.Admin, Path: s.fs.FilesRoot(u.Name),
				Files: files, Dirs: dirs, Used: bytes, TrashItems: ti, TrashSize: tb, LastScan: last,
				Devices: s.st.CountAppPasswords(u.Name)})
		}
		pg.Unclaimed = s.unclaimedFolders(known)
		shares, _ := s.st.ListShares()
		for _, sh := range shares {
			pg.Shares = append(pg.Shares, shareView{sh.ID, sh.Owner, sh.Path, sh.Target, sh.ReadOnly})
		}
		if j := s.currentJob(); j != nil {
			v := j.view()
			pg.Job, pg.Refresh = &v, v.Running
		}
		var defs []string
		for _, st := range s.topSizes(3) {
			pg.ReqSizes = append(pg.ReqSizes, fmt.Sprintf("%s (%d requests)", st.Spec, st.N))
			defs = append(defs, st.Spec.String())
		}
		if len(defs) == 0 {
			for _, z := range s.cfg.Preview.Pregen {
				defs = append(defs, strconv.Itoa(z))
			}
		}
		pg.DefaultSizes = strings.Join(defs, ", ")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := adminTpl.ExecuteTemplate(w, "admin.html", pg); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *Server) back(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, s.adminPrefix+"?m="+urlQ(msg), http.StatusSeeOther)
}

func urlQ(s string) string { return strings.ReplaceAll(template.URLQueryEscaper(s), "+", "%20") }

func (s *Server) adminPost(w http.ResponseWriter, r *http.Request, me, sess string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	op := strings.TrimPrefix(r.URL.Path, s.adminPrefix+"/")
	if op == "login" {
		ok, err := s.au.CheckPassword(r.FormValue("user"), r.FormValue("password"), s.clientIP(r))
		if err != nil {
			s.back(w, r, "Too many attempts, try later")
			return
		}
		if u, _ := s.st.GetUser(r.FormValue("user")); !ok || u == nil || !u.Admin {
			s.back(w, r, "Wrong credentials or not an admin")
			return
		}
		s.setSession(w, r, r.FormValue("user"))
		http.Redirect(w, r, s.adminPrefix, http.StatusSeeOther)
		return
	}
	if me == "" || !hmac.Equal([]byte(r.FormValue("_csrf")), []byte(s.csrf(sess))) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	switch op {
	case "logout":
		http.SetCookie(w, &http.Cookie{Name: "nclite_admin", Path: s.adminPrefix, MaxAge: -1})
		http.Redirect(w, r, s.adminPrefix, http.StatusSeeOther)

	case "user-add":
		name, pw := r.FormValue("name"), r.FormValue("password")
		switch {
		case !reUser.MatchString(name) || name == "." || name == "..":
			s.back(w, r, "Invalid user name (letters, digits, . _ @ - only)")
		case len(pw) < 8:
			s.back(w, r, "Password must be at least 8 characters")
		default:
			_, statErr := os.Stat(s.fs.FilesRoot(name))
			adopting := statErr == nil // a folder with files is already there
			if err := s.st.CreateUser(name, HashPassword(pw), r.FormValue("admin") == "1"); err != nil {
				s.back(w, r, "Cannot create user (already exists?)")
				return
			}
			s.fs.EnsureUser(name)
			if adopting && s.startJob(&Job{Kind: "scan", Users: []string{name}}) == nil {
				s.back(w, r, "User "+name+" created; indexing the existing files...")
				return
			}
			s.back(w, r, "User "+name+" created")
		}

	case "user-passwd":
		name, pw := r.FormValue("name"), r.FormValue("password")
		if len(pw) < 8 {
			s.back(w, r, "Password must be at least 8 characters")
			return
		}
		if err := s.st.SetPassword(name, HashPassword(pw)); err != nil {
			s.back(w, r, "No such user")
			return
		}
		s.au.InvalidatePasswords()
		s.back(w, r, "Password changed for "+name)

	case "user-revoke":
		s.st.DeleteAppPasswordsOf(r.FormValue("name"))
		s.back(w, r, "All devices of "+r.FormValue("name")+" signed out")

	case "user-delete":
		name := r.FormValue("name")
		if r.FormValue("confirm") != "1" {
			s.back(w, r, "Tick the confirmation box to delete a user")
			return
		}
		if name == me {
			s.back(w, r, "You cannot delete yourself")
			return
		}
		if err := s.st.DeleteUser(name); err != nil {
			s.back(w, r, "Delete failed")
			return
		}
		// The account goes; the files never do. This code path deliberately has no way
		// to remove a user's folder: it stays on disk and reappears under "Folders
		// without an account", so the account can be created again and picks it all up.
		s.fs.ReloadShares()
		s.back(w, r, "User "+name+" deleted; their files were left on the disk untouched")

	case "share-add":
		owner, target := r.FormValue("owner"), r.FormValue("target")
		rel, err := vfs.CleanRel(r.FormValue("path"))
		ou, _ := s.st.GetUser(owner)
		tu, _ := s.st.GetUser(target)
		switch {
		case ou == nil || tu == nil || owner == target:
			s.back(w, r, "Pick two different existing users")
		case err != nil || rel == "":
			s.back(w, r, "Enter a folder path inside the owner's files, e.g. Photos/Family")
		default:
			rel = s.fs.Canon(owner, rel)
			if fi, e := os.Stat(s.fs.Real(owner, rel)); e != nil || !fi.IsDir() {
				s.back(w, r, "That folder does not exist for "+owner)
				return
			}
			if e := s.st.AddShare(owner, rel, target, r.FormValue("ro") == "1"); e != nil {
				s.back(w, r, "Already shared")
				return
			}
			s.fs.ReloadShares()
			s.back(w, r, "Shared "+owner+"/"+rel+" with "+target)
		}

	case "job-scan", "job-previews":
		users, all, rel, err := s.jobTarget(r)
		if err != nil {
			s.back(w, r, err.Error())
			return
		}
		j := &Job{Kind: "scan", Users: users, AllUsrs: all, Path: rel}
		if op == "job-previews" {
			j.Kind = "previews"
			if j.Sizes, err = parseSizes(r.FormValue("sizes")); err != nil {
				s.back(w, r, err.Error())
				return
			}
			if !s.cfg.Preview.Enabled {
				s.back(w, r, "Previews are disabled in the config")
				return
			}
		}
		if err := s.startJob(j); err != nil {
			s.back(w, r, err.Error())
			return
		}
		s.back(w, r, "Job started")

	case "job-cancel":
		if j := s.currentJob(); j != nil && j.running() {
			j.cancel()
			s.back(w, r, "Cancelling after the current file...")
			return
		}
		s.back(w, r, "Nothing is running")

	case "share-delete":
		id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
		s.st.DeleteShare(id)
		s.fs.ReloadShares()
		s.back(w, r, "Share removed")

	default:
		http.NotFound(w, r)
	}
}

// unclaimedFolders lists directories under data_dir that look like a user's storage
// (they contain a "files" folder, which is also Nextcloud's layout) but have no account.
func (s *Server) unclaimedFolders(known map[string]bool) []string {
	ents, err := os.ReadDir(s.cfg.DataDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() || !reUser.MatchString(e.Name()) || known[e.Name()] {
			continue
		}
		if fi, err := os.Stat(filepath.Join(s.cfg.DataDir, e.Name(), "files")); err == nil && fi.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// jobTarget reads the user ("*" = everyone) and optional folder from a job form.
func (s *Server) jobTarget(r *http.Request) (users []string, all bool, rel string, err error) {
	rel, err = vfs.CleanRel(r.FormValue("path"))
	if err != nil {
		return nil, false, "", errors.New("invalid folder path")
	}
	if name := r.FormValue("user"); name == "*" {
		if rel != "" {
			return nil, false, "", errors.New("a folder can only be combined with one user")
		}
		list, _ := s.st.ListUsers()
		for _, u := range list {
			users = append(users, u.Name)
		}
		return users, true, "", nil
	} else if u, _ := s.st.GetUser(name); u != nil {
		return []string{name}, false, s.fs.Canon(name, rel), nil
	}
	return nil, false, "", errors.New("no such user")
}

// adminSecure decides whether the session cookie gets the Secure flag. It follows
// the scheme of the address the admin is opened at (admin_url if set, otherwise the
// request), never base_url: that one is the public https address of the file
// service, and a Secure cookie would never be stored on a plain http LAN address.
func (s *Server) adminSecure(r *http.Request) bool {
	if s.cfg.AdminURL != "" {
		return strings.HasPrefix(s.cfg.AdminURL, "https://")
	}
	if r.TLS != nil {
		return true
	}
	return s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https"
}
