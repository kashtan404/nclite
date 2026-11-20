package server

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"nclite/internal/store"
	"nclite/internal/vfs"
)

type davReq struct {
	s      *Server
	w      http.ResponseWriter
	r      *http.Request
	user   string
	rel    string // visible path, no slashes at the ends
	prefix string // URL prefix that maps to the user's root
	t      vfs.Target
}

func (s *Server) davOptions(w http.ResponseWriter) {
	w.Header().Set("DAV", "1, 3, extended-mkcol")
	w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, MKCOL, MOVE, COPY, PROPFIND, PROPPATCH, REPORT")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDAV(modern bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			s.davOptions(w)
			return
		}
		user, ok := s.authed(w, r)
		if !ok {
			return
		}
		var rel, prefix string
		if modern {
			rest := strings.TrimPrefix(r.URL.Path, "/remote.php/dav/files/")
			name, p, _ := strings.Cut(rest, "/")
			if name != user {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			rel, prefix = p, "/remote.php/dav/files/"+user
		} else {
			rel, prefix = strings.TrimPrefix(r.URL.Path, "/remote.php/webdav"), "/remote.php/webdav"
		}
		rel, err := vfs.CleanRel(rel)
		if err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		d := &davReq{s: s, w: w, r: r, user: user, rel: rel, prefix: prefix, t: s.fs.Resolve(user, rel)}
		// On a disk that ignores letter case, "photos" and "Photos" are one folder:
		// work with the spelling that is really on disk, never index the other one.
		d.t.Rel = s.fs.Canon(d.t.Owner, d.t.Rel)
		switch r.Method {
		case "PROPFIND":
			d.propfind()
		case http.MethodGet, http.MethodHead:
			d.get()
		case http.MethodPut:
			d.put()
		case "MKCOL":
			d.mkcol()
		case http.MethodDelete:
			d.del()
		case "MOVE", "COPY":
			d.moveCopy()
		case "PROPPATCH":
			d.proppatch()
		case "REPORT":
			d.report()
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	}
}

func (s *Server) handleDAVRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		s.davOptions(w)
		return
	}
	if p := strings.TrimSuffix(r.URL.Path, "/"); p != "/remote.php/dav" {
		http.NotFound(w, r)
		return
	}
	user, ok := s.authed(w, r)
	if !ok {
		return
	}
	if r.Method == "SEARCH" {
		s.davSearch(w, r, user)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	fmt.Fprint(w, xml.Header+`<d:multistatus xmlns:d="DAV:"><d:response><d:href>/remote.php/dav/</d:href>`+
		`<d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
}

func (d *davReq) status(code int) {
	d.w.Header().Set("Content-Length", "0")
	d.w.WriteHeader(code)
}

func (d *davReq) fail(err error) {
	code, msg := httpStatus(err)
	if code == http.StatusServiceUnavailable {
		d.w.Header().Set("Retry-After", "30")
	}
	http.Error(d.w, msg, code)
}

func (d *davReq) fileHeaders(n *store.Node) {
	h := d.w.Header()
	// set the map directly: Nextcloud clients look these up with exact casing
	h["OC-FileId"] = []string{d.s.fileIDString(n.ID)}
	h["OC-ETag"] = []string{`"` + n.ETag + `"`}
	h["ETag"] = []string{`"` + n.ETag + `"`}
}

func (d *davReq) destination() (string, bool) {
	u, err := url.Parse(d.r.Header.Get("Destination"))
	if err != nil || (u.Path != d.prefix && !strings.HasPrefix(u.Path, d.prefix+"/")) {
		return "", false
	}
	rel, err := vfs.CleanRel(strings.TrimPrefix(u.Path, d.prefix))
	return rel, err == nil
}

// ---- GET / HEAD ------------------------------------------------------------------

func (d *davReq) get() {
	n, err := d.s.fs.NodeFor(d.t.Owner, d.t.Rel)
	if err != nil {
		d.fail(err)
		return
	}
	if n.IsDir {
		http.Error(d.w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	f, err := os.Open(d.s.fs.Real(d.t.Owner, d.t.Rel))
	if err != nil {
		d.fail(err)
		return
	}
	defer f.Close()
	name := path.Base(d.rel)
	d.w.Header().Set("Content-Type", mimeOf(name))
	d.fileHeaders(n)
	http.ServeContent(d.w, d.r, name, time.Unix(n.Mtime, 0), f)
}

// ---- PUT -------------------------------------------------------------------------

// parentIsDir checks that the parent of rel exists and is a directory.
func (d *davReq) parentIsDir(owner, rel string) bool { return d.s.parentIsDir(owner, rel) }

func (d *davReq) put() {
	s := d.s
	if d.t.ReadOnly {
		http.Error(d.w, "Forbidden", http.StatusForbidden)
		return
	}
	if d.t.Rel == "" || d.t.MountRoot {
		http.Error(d.w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !d.parentIsDir(d.t.Owner, d.t.Rel) {
		http.Error(d.w, "Conflict", http.StatusConflict)
		return
	}
	real := s.fs.Real(d.t.Owner, d.t.Rel)
	existed := false
	if fi, err := os.Stat(real); err == nil {
		if fi.IsDir() {
			http.Error(d.w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		existed = true
	}
	if err := s.fs.EnsureUser(d.t.Owner); err != nil {
		d.fail(err)
		return
	}
	tmp, err := os.CreateTemp(s.fs.TmpDir(d.t.Owner), "put-*")
	if err != nil {
		d.fail(err)
		return
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := copyBuf(tmp, d.r.Body); err != nil {
		tmp.Close()
		return // client went away mid-upload
	}
	if err := tmp.Close(); err != nil {
		d.fail(err)
		return
	}
	accepted := false
	if v := d.r.Header.Get("X-OC-Mtime"); v != "" {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			t := time.Unix(sec, 0)
			if os.Chtimes(tmp.Name(), t, t) == nil {
				accepted = true
			}
		}
	}
	os.Chmod(tmp.Name(), 0o644)
	d.finishWrite(tmp.Name(), real, existed, accepted)
}

// finishWrite moves a finished upload from src into place, indexes it, sets the
// Nextcloud response headers and schedules preview generation. Renaming and indexing
// happen as one step with respect to folder renames and listings; otherwise a file
// landing in a folder that is being renamed at that very moment would be recorded
// under the old name. Returns false (after answering) on failure.
func (d *davReq) finishWrite(src, real string, existed, mtimeAccepted bool) bool {
	var n *store.Node
	err := d.s.fs.Structural(func() error {
		if err := os.Rename(src, real); err != nil {
			return err
		}
		fi, err := os.Stat(real)
		if err != nil {
			return err
		}
		n, err = d.s.st.SyncPath(d.t.Owner, d.t.Rel, fi)
		return err
	})
	if err != nil {
		d.fail(err)
		return false
	}
	d.fileHeaders(n)
	if mtimeAccepted {
		d.w.Header()["X-OC-MTime"] = []string{"accepted"}
	}
	d.s.pv.Enqueue(n.ID, n.ETag, real, path.Base(d.rel))
	if existed {
		d.status(http.StatusNoContent)
	} else {
		d.status(http.StatusCreated)
	}
	return true
}

// ---- MKCOL / DELETE --------------------------------------------------------------

func (d *davReq) mkcol() {
	n, err := d.s.doMkdir(d.user, d.rel)
	if err != nil {
		d.fail(err)
		return
	}
	d.fileHeaders(n)
	d.status(http.StatusCreated)
}

func (d *davReq) del() {
	if err := d.s.doDelete(d.user, d.rel); err != nil {
		d.fail(err)
		return
	}
	d.status(http.StatusNoContent)
}

// ---- MOVE / COPY -----------------------------------------------------------------

func (d *davReq) moveCopy() {
	if d.albumDestination() { // COPY of a photo into an album: no file is copied
		return
	}
	dstRel, ok := d.destination()
	if !ok {
		http.Error(d.w, "Bad Request", http.StatusBadRequest)
		return
	}
	move := d.r.Method == "MOVE"
	overwrite := !strings.EqualFold(d.r.Header.Get("Overwrite"), "F")
	n, existed, err := d.s.doMoveCopy(d.user, d.rel, dstRel, move, overwrite)
	if err != nil {
		d.fail(err)
		return
	}
	d.fileHeaders(n)
	if existed {
		d.status(http.StatusNoContent)
	} else {
		d.status(http.StatusCreated)
	}
}

func copyTree(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		ents, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range ents {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
	}
	if !fi.Mode().IsRegular() {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := copyBuf(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
}

// ---- PROPPATCH / REPORT ------------------------------------------------------------

func (d *davReq) emptyMultistatus() {
	io.Copy(io.Discard, io.LimitReader(d.r.Body, 1<<20))
	d.w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	d.w.WriteHeader(http.StatusMultiStatus)
	fmt.Fprint(d.w, xml.Header+`<d:multistatus xmlns:d="DAV:"></d:multistatus>`)
}

var nsPrefix = map[string]string{"DAV:": "d", "http://owncloud.org/ns": "oc", "http://nextcloud.org/ns": "nc"}

// ---- PROPFIND ----------------------------------------------------------------------

type entry struct {
	fav    bool
	href   string
	n      *store.Node
	etag   string
	size   int64
	ro     bool
	isMnt  bool
	hasPrv bool
}

func (d *davReq) propfind() {
	s := d.s
	io.Copy(io.Discard, io.LimitReader(d.r.Body, 1<<20)) // we always answer with the full property set
	depth := d.r.Header.Get("Depth")
	favs := s.st.FavoriteSet(d.user)

	self, err := s.fs.NodeFor(d.t.Owner, d.t.Rel)
	if err != nil {
		d.fail(err)
		return
	}
	var kids []*store.Node
	if self.IsDir && depth != "0" {
		if kids, err = s.fs.ListDir(d.t.Owner, d.t.Rel); err != nil {
			d.fail(err)
			return
		}
		if n, err := s.st.NodeByPath(d.t.Owner, d.t.Rel); err == nil && n != nil {
			self = n // etag may have moved during reconcile
		}
	}

	atUserRoot := d.rel == "" && !d.t.InMount
	var mounts []vfs.Mount
	var mountNodes []*store.Node
	if atUserRoot {
		for _, m := range s.fs.Mounts(d.user) {
			if n, err := s.fs.NodeFor(m.Owner, m.Path); err == nil && n.IsDir {
				mounts, mountNodes = append(mounts, m), append(mountNodes, n)
			}
		}
	}

	w := d.w
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	bw := bufio.NewWriterSize(w, 32*1024)
	defer bw.Flush()
	bw.WriteString(xml.Header)
	bw.WriteString(`<d:multistatus xmlns:d="DAV:" xmlns:s="http://sabredav.org/ns" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">`)

	// the entry for the requested resource itself
	e := entry{fav: favs[self.ID], href: s.href(d.prefix, d.rel, self.IsDir), n: self, etag: self.ETag, ro: d.t.ReadOnly, isMnt: d.t.MountRoot}
	if self.IsDir {
		e.size = s.st.DirSize(self.Owner, self.Path)
		if atUserRoot && len(mounts) > 0 {
			h := sha256.New()
			h.Write([]byte(self.ETag))
			for i, n := range mountNodes {
				h.Write([]byte(n.ETag))
				e.size += s.st.DirSize(mounts[i].Owner, mounts[i].Path)
			}
			e.etag = hex.EncodeToString(h.Sum(nil))[:16]
		}
	} else {
		e.size, e.hasPrv = self.Size, s.pv.Supported(self.Path)
	}
	s.writeEntry(bw, e)

	for _, k := range kids {
		name := path.Base(k.Path)
		ke := entry{fav: favs[k.ID], href: s.href(d.prefix, store.Join(d.rel, name), k.IsDir), n: k, etag: k.ETag, ro: d.t.ReadOnly}
		if k.IsDir {
			ke.size = s.st.DirSize(k.Owner, k.Path)
		} else {
			ke.size, ke.hasPrv = k.Size, s.pv.Supported(name)
		}
		s.writeEntry(bw, ke)
	}
	if depth != "0" {
		for i, m := range mounts {
			n := mountNodes[i]
			s.writeEntry(bw, entry{fav: favs[n.ID], href: s.href(d.prefix, m.Name, true), n: n, etag: n.ETag,
				size: s.st.DirSize(n.Owner, n.Path), ro: m.ReadOnly, isMnt: true})
		}
	}
	bw.WriteString(`</d:multistatus>`)
}

func (s *Server) href(prefix, rel string, isDir bool) string {
	var b strings.Builder
	b.WriteString(prefix)
	if rel != "" {
		for _, seg := range strings.Split(rel, "/") {
			b.WriteByte('/')
			b.WriteString(url.PathEscape(seg))
		}
	}
	if isDir {
		b.WriteByte('/')
	}
	return b.String()
}

func perms(e entry) string {
	switch {
	case e.ro:
		return "G"
	case e.isMnt:
		return "GCK" // the recipient may add files inside a shared folder but not rename or delete the mount
	case e.n.IsDir:
		return "RGDNVCK"
	}
	return "RGDNVW"
}

var xmlEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

func xesc(s string) string { return xmlEsc.Replace(s) }

func (s *Server) writeEntry(bw *bufio.Writer, e entry) {
	n := e.n
	bw.WriteString(`<d:response><d:href>`)
	bw.WriteString(xesc(e.href))
	bw.WriteString(`</d:href><d:propstat><d:prop>`)
	bw.WriteString(`<d:getlastmodified>` + time.Unix(n.Mtime, 0).UTC().Format(http.TimeFormat) + `</d:getlastmodified>`)
	if n.IsDir {
		bw.WriteString(`<d:resourcetype><d:collection/></d:resourcetype>`)
		bw.WriteString(`<d:quota-used-bytes>` + strconv.FormatInt(e.size, 10) + `</d:quota-used-bytes><d:quota-available-bytes>-3</d:quota-available-bytes>`)
	} else {
		bw.WriteString(`<d:resourcetype/>`)
		bw.WriteString(`<d:getcontentlength>` + strconv.FormatInt(n.Size, 10) + `</d:getcontentlength>`)
		bw.WriteString(`<d:getcontenttype>` + xesc(mimeOf(path.Base(n.Path))) + `</d:getcontenttype>`)
	}
	bw.WriteString(`<d:getetag>"` + e.etag + `"</d:getetag>`)
	bw.WriteString(`<oc:id>` + s.fileIDString(n.ID) + `</oc:id>`)
	bw.WriteString(`<oc:fileid>` + strconv.FormatInt(n.ID, 10) + `</oc:fileid>`)
	bw.WriteString(`<oc:permissions>` + perms(e) + `</oc:permissions>`)
	bw.WriteString(`<oc:size>` + strconv.FormatInt(e.size, 10) + `</oc:size>`)
	bw.WriteString(`<oc:favorite>` + strconv.Itoa(b2i(e.fav)) + `</oc:favorite>`)
	bw.WriteString(`<oc:owner-id>` + xesc(n.Owner) + `</oc:owner-id>`)
	bw.WriteString(`<oc:owner-display-name>` + xesc(n.Owner) + `</oc:owner-display-name>`)
	bw.WriteString(`<oc:comments-unread>0</oc:comments-unread>`)
	bw.WriteString(`<oc:share-types/>`)
	bw.WriteString(`<nc:has-preview>` + strconv.FormatBool(e.hasPrv) + `</nc:has-preview>`)
	bw.WriteString(`<nc:is-encrypted>0</nc:is-encrypted>`)
	bw.WriteString(`<nc:creation_time>` + strconv.FormatInt(n.Mtime, 10) + `</nc:creation_time>`)
	bw.WriteString(`<nc:upload_time>` + strconv.FormatInt(n.Mtime, 10) + `</nc:upload_time>`)
	bw.WriteString(`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
