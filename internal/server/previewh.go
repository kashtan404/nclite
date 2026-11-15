package server

import (
	"net/http"
	"os"
	"path"
	"strconv"
	"time"

	"nclite/internal/store"
	"nclite/internal/vfs"
)

// handlePreview serves /index.php/core/preview?fileId=..&x=..&y=..&a=1[&mode=cover]
// and the older ?file=/path form. Previews are generated on the server and cached.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authed(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var n *store.Node
	var err error
	if id, perr := strconv.ParseInt(q.Get("fileId"), 10, 64); perr == nil {
		n, err = s.st.NodeByID(id)
		if err == nil && n != nil {
			if _, ok := s.fs.CanAccess(user, n); !ok {
				n = nil
			}
		}
	} else if f := q.Get("file"); f != "" {
		if rel, cerr := vfs.CleanRel(f); cerr == nil {
			t := s.fs.Resolve(user, rel)
			n, err = s.fs.NodeFor(t.Owner, t.Rel)
		}
	}
	if err != nil || n == nil || n.IsDir {
		http.NotFound(w, r)
		return
	}
	// make sure the index matches the file on disk (etag keys the cache)
	if fresh, err := s.fs.NodeFor(n.Owner, n.Path); err == nil {
		n = fresh
	} else {
		http.NotFound(w, r)
		return
	}
	s.servePreview(w, r, n, path.Base(n.Path), s.fs.Real(n.Owner, n.Path))
}

// servePreview renders (or fetches from cache) the preview of n and writes it.
// name decides the file type, src is where the bytes are on disk.
func (s *Server) servePreview(w http.ResponseWriter, r *http.Request, n *store.Node, name, src string) {
	q := r.URL.Query()
	if !s.pv.Supported(name) {
		http.NotFound(w, r)
		return
	}
	x, _ := strconv.Atoi(q.Get("x"))
	y, _ := strconv.Atoi(q.Get("y"))
	keepAspect := q.Get("a") == "1" || q.Get("a") == "true"
	cover := q.Get("mode") == "cover" && !keepAspect
	s.noteSize(x, y, cover)
	file, err := s.pv.Get(n.ID, n.ETag, src, name, x, y, cover)
	if err != nil {
		logPreviewErr(name, err)
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("ETag", `"`+n.ETag+`-`+strconv.Itoa(x)+"x"+strconv.Itoa(y)+`"`)
	http.ServeContent(w, r, "", time.Time{}, f)
}
