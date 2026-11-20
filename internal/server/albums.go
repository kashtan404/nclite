package server

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"nclite/internal/store"
)

// Photo albums, in the shape of the Nextcloud Photos app:
//
//	MKCOL    /remote.php/dav/photos/<user>/albums/<name>/        create
//	PROPFIND /remote.php/dav/photos/<user>/albums/               list albums
//	PROPFIND /remote.php/dav/photos/<user>/albums/<name>/        list the photos of an album
//	MOVE     .../albums/<name>/ -> .../albums/<new>/              rename
//	DELETE   .../albums/<name>/                                   delete the album (files stay)
//	COPY     /remote.php/dav/files/<user>/<file> -> .../albums/<name>/<file>   add a photo
//	DELETE   .../albums/<name>/<id>-<file>                        remove a photo from the album
//
// An album only lists files (by file id); nothing is ever copied on disk.

const photosPrefix = "/remote.php/dav/photos/"

// albumNodes returns the album's files this user can still reach, in the order they
// were added (files that were deleted, trashed or are no longer shared are skipped).
func (s *Server) albumNodes(user string, a *store.Album) []*store.Node {
	items, err := s.st.AlbumItems(a.ID)
	if err != nil {
		return nil
	}
	out := make([]*store.Node, 0, len(items))
	for _, n := range items {
		if n.IsDir {
			continue
		}
		if _, ok := s.fs.CanAccess(user, n); ok {
			out = append(out, n)
		}
	}
	return out
}

type albumStats struct {
	count, last int64
	dateRange   string
}

func albumStatsOf(nodes []*store.Node) albumStats {
	st := albumStats{count: int64(len(nodes)), last: -1}
	if len(nodes) == 0 {
		return st
	}
	st.last = nodes[len(nodes)-1].ID
	lo, hi := nodes[0].Mtime, nodes[0].Mtime
	for _, n := range nodes {
		lo, hi = min(lo, n.Mtime), max(hi, n.Mtime)
	}
	st.dateRange = fmt.Sprintf(`{"start":%d,"end":%d}`, lo, hi)
	return st
}

func photoItemName(n *store.Node) string {
	return strconv.FormatInt(n.ID, 10) + "-" + path.Base(n.Path)
}

// parsePhotoItem extracts the file id from "<id>-<file name>".
func parsePhotoItem(name string) (int64, bool) {
	i := strings.IndexByte(name, '-')
	if i <= 0 {
		return 0, false
	}
	id, err := strconv.ParseInt(name[:i], 10, 64)
	return id, err == nil
}

func validAlbumName(name string) bool { return validName(name) }

func (s *Server) handlePhotos(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		s.davOptions(w)
		return
	}
	user, ok := s.authed(w, r)
	if !ok {
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, photosPrefix), "/"), "/")
	if parts[0] != user {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	base := photosPrefix + user
	depth := r.Header.Get("Depth")
	kind := ""
	if len(parts) > 1 {
		kind = parts[1]
	}
	switch {
	case kind == "" || kind == "sharedalbums":
		// albums shared between users are not supported: an empty collection
		if r.Method != "PROPFIND" {
			http.NotFound(w, r)
			return
		}
		s.collectionOnly(w, r, base+"/"+kind)
	case kind != "albums":
		http.NotFound(w, r)
	case len(parts) == 2:
		if r.Method != "PROPFIND" {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.albumList(w, r, user, base, depth)
	case len(parts) == 3:
		s.albumCollection(w, r, user, base, parts[2], depth)
	default:
		s.albumItem(w, r, user, base, parts[2], strings.Join(parts[3:], "/"))
	}
}

func (s *Server) collectionOnly(w http.ResponseWriter, r *http.Request, href string) {
	io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	fmt.Fprint(w, xml.Header+`<d:multistatus xmlns:d="DAV:"><d:response><d:href>`+xesc(href)+`/</d:href>`+
		`<d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
}

const msOpen = `<d:multistatus xmlns:d="DAV:" xmlns:s="http://sabredav.org/ns" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">`

func writeAlbumEntry(bw *bufio.Writer, href string, a *store.Album, st albumStats) {
	bw.WriteString(`<d:response><d:href>` + xesc(href) + `</d:href><d:propstat><d:prop>`)
	bw.WriteString(`<d:resourcetype><d:collection/></d:resourcetype>`)
	bw.WriteString(`<d:getlastmodified>` + time.Unix(a.Created, 0).UTC().Format(http.TimeFormat) + `</d:getlastmodified>`)
	bw.WriteString(`<nc:last-photo>` + strconv.FormatInt(st.last, 10) + `</nc:last-photo>`)
	bw.WriteString(`<nc:nbItems>` + strconv.FormatInt(st.count, 10) + `</nc:nbItems>`)
	bw.WriteString(`<nc:location></nc:location>`)
	bw.WriteString(`<nc:dateRange>` + xesc(st.dateRange) + `</nc:dateRange>`)
	bw.WriteString(`<nc:collaborators>[]</nc:collaborators>`)
	bw.WriteString(`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
}

func (s *Server) multistatus(w http.ResponseWriter, fn func(bw *bufio.Writer)) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	bw := bufio.NewWriterSize(w, 32*1024)
	defer bw.Flush()
	bw.WriteString(xml.Header + msOpen)
	fn(bw)
	bw.WriteString(`</d:multistatus>`)
}

func albumHref(base, name string) string { return base + "/albums/" + url.PathEscape(name) + "/" }

func (s *Server) albumList(w http.ResponseWriter, r *http.Request, user, base, depth string) {
	io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	albums, err := s.st.ListAlbums(user)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	s.multistatus(w, func(bw *bufio.Writer) {
		// the collection itself first (the client skips the entry whose URL equals the request URL)
		bw.WriteString(`<d:response><d:href>` + xesc(base+"/albums/") + `</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
		if depth == "0" {
			return
		}
		for i := range albums {
			a := &albums[i]
			writeAlbumEntry(bw, albumHref(base, a.Name), a, albumStatsOf(s.albumNodes(user, a)))
		}
	})
}

func (s *Server) albumCollection(w http.ResponseWriter, r *http.Request, user, base, name, depth string) {
	if r.Method == "MKCOL" {
		if !validAlbumName(name) {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		if _, err := s.st.CreateAlbum(user, name); err != nil {
			if err == store.ErrExists {
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed) // as on a real server: it exists
				return
			}
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusCreated)
		return
	}
	a, err := s.st.AlbumByName(user, name)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if a == nil {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case "PROPFIND":
		io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		nodes := s.albumNodes(user, a)
		favs := s.st.FavoriteSet(user)
		s.multistatus(w, func(bw *bufio.Writer) {
			writeAlbumEntry(bw, albumHref(base, a.Name), a, albumStatsOf(nodes))
			if depth == "0" {
				return
			}
			for _, n := range nodes {
				s.writeEntry(bw, s.albumEntry(user, base, a, n, favs))
			}
		})
	case http.MethodDelete:
		if err := s.st.DeleteAlbum(a.ID); err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNoContent)
	case "MOVE": // rename
		dest, err := url.Parse(r.Header.Get("Destination"))
		prefix := base + "/albums/"
		if err != nil || !strings.HasPrefix(dest.Path, prefix) {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		newName := strings.Trim(strings.TrimPrefix(dest.Path, prefix), "/")
		if !validAlbumName(newName) {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		if err := s.st.RenameAlbum(a.ID, newName); err != nil {
			if err == store.ErrExists {
				http.Error(w, "Precondition Failed", http.StatusPreconditionFailed)
				return
			}
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusCreated)
	case "PROPPATCH": // location, collaborators, ...: accepted, not stored
		io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		s.multistatus(w, func(bw *bufio.Writer) {
			bw.WriteString(`<d:response><d:href>` + xesc(albumHref(base, a.Name)) + `</d:href><d:propstat><d:prop/><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
		})
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) albumEntry(user, base string, a *store.Album, n *store.Node, favs map[int64]bool) entry {
	ro, _ := s.fs.CanAccess(user, n)
	name := path.Base(n.Path)
	return entry{fav: favs[n.ID], href: albumHref(base, a.Name) + url.PathEscape(photoItemName(n)), n: n,
		etag: n.ETag, size: n.Size, ro: ro, hasPrv: s.pv.Supported(name)}
}

func (s *Server) albumItem(w http.ResponseWriter, r *http.Request, user, base, albumName, item string) {
	a, err := s.st.AlbumByName(user, albumName)
	if err != nil || a == nil {
		http.NotFound(w, r)
		return
	}
	id, ok := parsePhotoItem(item)
	if !ok {
		http.NotFound(w, r)
		return
	}
	n, err := s.st.NodeByID(id)
	if err != nil || n == nil {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		removed, err := s.st.AlbumRemove(a.ID, id)
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		if !removed {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNoContent)
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
	switch r.Method {
	case "PROPFIND":
		io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		favs := s.st.FavoriteSet(user)
		s.multistatus(w, func(bw *bufio.Writer) { s.writeEntry(bw, s.albumEntry(user, base, a, fresh, favs)) })
	case http.MethodGet, http.MethodHead:
		f, err := os.Open(s.fs.Real(fresh.Owner, fresh.Path))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", mimeOf(path.Base(fresh.Path)))
		http.ServeContent(w, r, path.Base(fresh.Path), time.Unix(fresh.Mtime, 0), f)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// albumDestination handles COPY whose Destination is inside a photo album: the
// client copies a photo "into" the album, which here means adding it to the list.
// Returns false when the destination is an ordinary file path.
func (d *davReq) albumDestination() bool {
	u, err := url.Parse(d.r.Header.Get("Destination"))
	if err != nil || !strings.HasPrefix(u.Path, photosPrefix) {
		return false
	}
	if d.r.Method != "COPY" {
		http.Error(d.w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return true
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(u.Path, photosPrefix), "/"), "/")
	if len(parts) < 3 || parts[0] != d.user || parts[1] != "albums" {
		http.Error(d.w, "Forbidden", http.StatusForbidden)
		return true
	}
	a, err := d.s.st.AlbumByName(d.user, parts[2])
	if err != nil {
		http.Error(d.w, "Internal Server Error", http.StatusInternalServerError)
		return true
	}
	if a == nil {
		http.NotFound(d.w, d.r)
		return true
	}
	n, err := d.s.fs.NodeFor(d.t.Owner, d.t.Rel)
	if err != nil {
		d.fail(err)
		return true
	}
	if n.IsDir {
		http.Error(d.w, "Only files can be added to an album", http.StatusForbidden)
		return true
	}
	added, err := d.s.st.AlbumAdd(a.ID, n.ID)
	if err != nil {
		http.Error(d.w, "Internal Server Error", http.StatusInternalServerError)
		return true
	}
	if added {
		d.status(http.StatusCreated)
	} else {
		d.status(http.StatusNoContent) // it was already there
	}
	return true
}

var reSearchID = regexp.MustCompile(`literal>\s*([0-9]+)\s*<`)

// davSearch answers the one SEARCH the iOS client sends that matters here: "find the
// file with this id" (used to open an album photo, a notification or a link). Any other
// search gets an empty result.
func (s *Server) davSearch(w http.ResponseWriter, r *http.Request, user string) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var found *entry
	if m := reSearchID.FindSubmatch(body); m != nil && strings.Contains(string(body), "fileid") {
		id, _ := strconv.ParseInt(string(m[1]), 10, 64)
		if n, err := s.st.NodeByID(id); err == nil && n != nil {
			if ro, ok := s.fs.CanAccess(user, n); ok {
				if rel, _, ok := s.fs.VisiblePath(user, n); ok {
					if fresh, err := s.fs.NodeFor(n.Owner, n.Path); err == nil {
						e := entry{fav: s.st.FavoriteSet(user)[fresh.ID], n: fresh, etag: fresh.ETag, ro: ro,
							href: s.href("/remote.php/dav/files/"+user, rel, fresh.IsDir)}
						if fresh.IsDir {
							e.size = s.st.DirSize(fresh.Owner, fresh.Path)
						} else {
							e.size, e.hasPrv = fresh.Size, s.pv.Supported(path.Base(fresh.Path))
						}
						found = &e
					}
				}
			}
		}
	}
	s.multistatus(w, func(bw *bufio.Writer) {
		if found != nil {
			s.writeEntry(bw, *found)
		}
	})
}
