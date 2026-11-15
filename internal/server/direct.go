package server

import (
	"net/http"
	"os"
	"path"
	"strconv"
	"time"

	"nclite/internal/store"
)

const maxDirectTTL = 24 * time.Hour

// ocsDirect implements POST /ocs/v2.php/apps/dav/api/v1/direct. The iOS app asks
// for one of these links before it plays a video: AVPlayer cannot send Basic auth
// headers, so it streams from an unguessable, expiring URL instead.
func (s *Server) ocsDirect(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authed(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		ocsReply(w, r, http.StatusMethodNotAllowed, nil)
		return
	}
	id, err := strconv.ParseInt(r.FormValue("fileId"), 10, 64)
	if err != nil {
		ocsReply(w, r, http.StatusBadRequest, nil)
		return
	}
	n, err := s.st.NodeByID(id)
	if err != nil || n == nil || n.IsDir {
		ocsReply(w, r, http.StatusNotFound, nil)
		return
	}
	if _, ok := s.fs.CanAccess(user, n); !ok {
		ocsReply(w, r, http.StatusForbidden, nil)
		return
	}
	ttl := 8 * time.Hour // Nextcloud's default
	if v, err := strconv.Atoi(r.FormValue("expirationTime")); err == nil && v > 0 {
		ttl = min(time.Duration(v)*time.Second, maxDirectTTL)
	}
	token := store.RandHex(24)
	if err := s.st.AddDirect(token, user, n.ID, time.Now().Add(ttl)); err != nil {
		ocsReply(w, r, http.StatusInternalServerError, nil)
		return
	}
	ocsReply(w, r, http.StatusOK, map[string]any{"url": s.baseURL(r) + "/remote.php/direct/" + token})
}

// handleDirect serves a file for a direct-download token. No credentials are
// needed; the token is the credential. Range requests work, so video can seek.
func (s *Server) handleDirect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.URL.Path[len("/remote.php/direct/"):]
	user, id, ok := s.st.GetDirect(token)
	if !ok {
		http.NotFound(w, r)
		return
	}
	n, err := s.st.NodeByID(id)
	if err != nil || n == nil || n.IsDir {
		http.NotFound(w, r)
		return
	}
	if _, ok := s.fs.CanAccess(user, n); !ok { // share removed since the link was made
		http.NotFound(w, r)
		return
	}
	fresh, err := s.fs.NodeFor(n.Owner, n.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(s.fs.Real(fresh.Owner, fresh.Path))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	name := path.Base(fresh.Path)
	w.Header().Set("Content-Type", mimeOf(name))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("ETag", `"`+fresh.ETag+`"`)
	http.ServeContent(w, r, name, time.Unix(fresh.Mtime, 0), f)
}
