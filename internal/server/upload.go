package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"nclite/internal/store"
	"nclite/internal/vfs"
)

var (
	reUploadID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	reChunk    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// handleUploads implements Nextcloud chunked upload v2:
//
//	MKCOL  /remote.php/dav/uploads/<user>/<id>            start
//	PUT    /remote.php/dav/uploads/<user>/<id>/<chunk>    upload a piece
//	MOVE   /remote.php/dav/uploads/<user>/<id>/.file      assemble into Destination
//	DELETE /remote.php/dav/uploads/<user>/<id>            abort
func (s *Server) handleUploads(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		s.davOptions(w)
		return
	}
	user, ok := s.authed(w, r)
	if !ok {
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/remote.php/dav/uploads/"), "/", 3)
	if parts[0] != user {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if len(parts) < 2 || parts[1] == "" {
		if r.Method == "PROPFIND" {
			s.emptyUploadList(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	id, name := parts[1], ""
	if len(parts) == 3 {
		name = strings.TrimSuffix(parts[2], "/")
	}
	if !reUploadID.MatchString(id) || (name != "" && name != ".file" && !reChunk.MatchString(name)) {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(s.fs.TmpDir(user), "chunks", id)
	empty := func(code int) { w.Header().Set("Content-Length", "0"); w.WriteHeader(code) }

	switch {
	case r.Method == "MKCOL" && name == "":
		if err := s.fs.EnsureUser(user); err != nil || os.MkdirAll(dir, 0o755) != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		empty(http.StatusCreated)

	case r.Method == http.MethodPut && name != "" && name != ".file":
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		part := filepath.Join(dir, name+".part")
		f, err := os.Create(part)
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		_, err = copyBuf(f, r.Body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(part)
			return
		}
		if os.Rename(part, filepath.Join(dir, name)) != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		empty(http.StatusCreated)

	case r.Method == "MOVE" && name == ".file":
		s.assemble(w, r, user, dir)

	case r.Method == http.MethodDelete && name == "":
		os.RemoveAll(dir)
		empty(http.StatusNoContent)

	case r.Method == "PROPFIND" && name == "":
		s.listChunks(w, r, user, id, dir)

	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) emptyUploadList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	fmt.Fprint(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"></d:multistatus>`)
}

func (s *Server) listChunks(w http.ResponseWriter, r *http.Request, user, id, dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	base := "/remote.php/dav/uploads/" + user + "/" + id
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response><d:href>` + base + `/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
	if r.Header.Get("Depth") != "0" {
		for _, e := range ents {
			info, err := e.Info()
			if err != nil || !info.Mode().IsRegular() || strings.HasSuffix(e.Name(), ".part") {
				continue
			}
			fmt.Fprintf(&b, `<d:response><d:href>%s/%s</d:href><d:propstat><d:prop><d:getcontentlength>%d</d:getcontentlength><d:resourcetype/></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`,
				base, xesc(e.Name()), info.Size())
		}
	}
	b.WriteString(`</d:multistatus>`)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	io.WriteString(w, b.String())
}

// assemble joins the chunks and moves the result to the Destination. The first
// chunk is renamed into place and the rest are appended to it, so the file is
// never held twice on disk.
func (s *Server) assemble(w http.ResponseWriter, r *http.Request, user, dir string) {
	// reuse the files-DAV destination logic: Destination points into dav/files/<user>/
	d := &davReq{s: s, w: w, r: r, user: user, prefix: "/remote.php/dav/files/" + user}
	dstRel, ok := d.destination()
	if !ok || dstRel == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	d.rel, d.t = dstRel, s.fs.Resolve(user, dstRel)
	d.t.Rel = s.fs.Canon(d.t.Owner, d.t.Rel)
	if d.t.ReadOnly || d.t.MountRoot {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if !d.parentIsDir(d.t.Owner, d.t.Rel) {
		http.Error(w, "Conflict", http.StatusConflict)
		return
	}
	real := s.fs.Real(d.t.Owner, d.t.Rel)
	existed := false
	if fi, err := os.Stat(real); err == nil {
		if fi.IsDir() {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		existed = true
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var names []string
	numeric := true
	for _, e := range ents {
		if e.Type().IsRegular() && e.Name() != ".file" && !strings.HasSuffix(e.Name(), ".part") {
			names = append(names, e.Name())
			if _, err := strconv.ParseInt(e.Name(), 10, 64); err != nil {
				numeric = false
			}
		}
	}
	if len(names) == 0 {
		http.Error(w, "Conflict: no chunks", http.StatusConflict)
		return
	}
	sort.Slice(names, func(i, j int) bool {
		if numeric {
			a, _ := strconv.ParseInt(names[i], 10, 64)
			b, _ := strconv.ParseInt(names[j], 10, 64)
			return a < b
		}
		return names[i] < names[j]
	})

	if err := s.fs.EnsureUser(d.t.Owner); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	asm := filepath.Join(s.fs.TmpDir(d.t.Owner), "asm-"+store.RandHex(6))
	defer os.Remove(asm)
	if err := os.Rename(filepath.Join(dir, names[0]), asm); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	out, err := os.OpenFile(asm, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	for _, n := range names[1:] {
		in, err := os.Open(filepath.Join(dir, n))
		if err == nil {
			_, err = copyBuf(out, in)
			in.Close()
		}
		if err != nil {
			out.Close()
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		os.Remove(filepath.Join(dir, n))
	}
	if err := out.Close(); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if v := r.Header.Get("OC-Total-Length"); v != "" {
		if want, err := strconv.ParseInt(v, 10, 64); err == nil {
			if fi, err := os.Stat(asm); err == nil && fi.Size() != want {
				http.Error(w, fmt.Sprintf("size mismatch: got %d, expected %d", fi.Size(), want), http.StatusBadRequest)
				return
			}
		}
	}
	accepted := false
	if v := r.Header.Get("X-OC-Mtime"); v != "" {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			t := time.Unix(sec, 0)
			accepted = os.Chtimes(asm, t, t) == nil
		}
	}
	os.Chmod(asm, 0o644)
	if d.finishWrite(asm, real, existed, accepted) {
		os.RemoveAll(dir) // chunks are only dropped once the file is safely in place
	}
}

var _ = path.Base
var _ = vfs.CleanRel
