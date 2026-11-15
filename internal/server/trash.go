package server

import (
	"bufio"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"nclite/internal/store"
)

// removeToTrash moves a file or folder into the owner's trash. It is a rename on
// the same filesystem, so even a huge folder is "deleted" instantly, and its index
// rows (file ids, favorites) are kept so a restore brings everything back intact.
func (s *Server) removeToTrash(owner, rel string) error {
	if rel == "" || rel == "." || rel == "/" {
		// last line of defence: nothing may ever remove or trash a user's whole folder
		return errors.New("refusing to remove a user's root folder")
	}
	real := s.fs.Real(owner, rel)
	if s.cfg.TrashDays < 0 { // trash disabled
		return s.fs.Structural(func() error {
			if err := os.RemoveAll(real); err != nil {
				return err
			}
			return s.st.DeletePath(owner, rel)
		})
	}
	n, err := s.fs.NodeFor(owner, rel)
	if err != nil {
		return err
	}
	size := n.Size
	if n.IsDir {
		size = s.st.DirSize(owner, rel)
	}
	if err := s.fs.EnsureUser(owner); err != nil {
		return err
	}
	it, err := s.st.TrashAdd(owner, path.Base(rel), store.ParentOf(rel), n.IsDir, size)
	if err != nil {
		return err
	}
	return s.fs.Structural(func() error {
		if err := os.Rename(real, s.fs.TrashPath(owner, it.ID)); err != nil {
			s.st.TrashDelete(it.ID)
			return err
		}
		return s.st.RekeyTree(owner, rel, store.TrashOwner(owner), strconv.FormatInt(it.ID, 10))
	})
}

func uniqueName(dirReal, name string) string {
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	cand := name
	for i := 1; ; i++ {
		if _, err := os.Lstat(filepath.Join(dirReal, cand)); errors.Is(err, fs.ErrNotExist) {
			return cand
		}
		if i == 1 {
			cand = base + " (restored)" + ext
		} else {
			cand = fmt.Sprintf("%s (restored %d)%s", base, i, ext)
		}
	}
}

// trashRestore puts an item back where it was deleted from. If that folder is gone
// the item goes to the root; if the name is taken it gets " (restored)" appended.
func (s *Server) trashRestore(it *store.TrashItem) (string, error) {
	owner := it.Owner
	dir := it.OrigDir
	if dir != "" {
		if fi, err := os.Stat(s.fs.Real(owner, dir)); err != nil || !fi.IsDir() {
			dir = ""
		}
	}
	if err := s.fs.EnsureUser(owner); err != nil {
		return "", err
	}
	name := uniqueName(s.fs.Real(owner, dir), it.Name)
	dest := store.Join(dir, name)
	tid := strconv.FormatInt(it.ID, 10)
	err := s.fs.Structural(func() error {
		if err := os.Rename(s.fs.TrashPath(owner, it.ID), s.fs.Real(owner, dest)); err != nil {
			return err
		}
		return s.st.RekeyTree(store.TrashOwner(owner), tid, owner, dest)
	})
	if err != nil {
		return "", err
	}
	return dest, s.st.TrashDelete(it.ID)
}

func (s *Server) trashPurge(it store.TrashItem) error {
	if err := os.RemoveAll(s.fs.TrashPath(it.Owner, it.ID)); err != nil {
		return err
	}
	if err := s.st.PurgeTree(store.TrashOwner(it.Owner), strconv.FormatInt(it.ID, 10)); err != nil {
		return err
	}
	return s.st.TrashDelete(it.ID)
}

// purgeTrash removes items older than trash_days (called from the hourly job).
func (s *Server) purgeTrash() {
	if s.cfg.TrashDays <= 0 {
		return
	}
	items, err := s.st.TrashOlderThan(time.Now().Add(-time.Duration(s.cfg.TrashDays) * 24 * time.Hour).Unix())
	if err != nil {
		return
	}
	for _, it := range items {
		s.trashPurge(it)
	}
}

// handleTrash serves the Nextcloud trashbin WebDAV API:
//
//	PROPFIND /remote.php/dav/trashbin/<user>/trash[/<item>]   list
//	MOVE     .../trash/<item>  -> .../restore/...              restore to original place
//	DELETE   .../trash/<item>                                  delete permanently
//	DELETE   .../trash                                         empty the trash
func (s *Server) handleTrash(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		s.davOptions(w)
		return
	}
	user, ok := s.authed(w, r)
	if !ok {
		return
	}
	parts := strings.SplitN(strings.Trim(strings.TrimPrefix(r.URL.Path, "/remote.php/dav/trashbin/"), "/"), "/", 3)
	if parts[0] != user {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	what, item := "", ""
	if len(parts) > 1 {
		what = parts[1]
	}
	if len(parts) > 2 {
		item = parts[2]
	}
	prefix := "/remote.php/dav/trashbin/" + user
	empty := func(code int) { w.Header().Set("Content-Length", "0"); w.WriteHeader(code) }

	switch {
	case what == "" || what == "restore":
		if r.Method != "PROPFIND" {
			http.NotFound(w, r)
			return
		}
		href := prefix + "/"
		if what != "" {
			href += what + "/"
		}
		s.trashCollection(w, r, href, nil, user)

	case what == "trash" && item == "":
		switch r.Method {
		case "PROPFIND":
			items, err := s.st.TrashList(user)
			if err != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			if r.Header.Get("Depth") == "0" {
				items = nil
			}
			s.trashCollection(w, r, prefix+"/trash/", items, user)
		case http.MethodDelete:
			items, _ := s.st.TrashList(user)
			for _, it := range items {
				if err := s.trashPurge(it); err != nil {
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
					return
				}
			}
			empty(http.StatusNoContent)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}

	case what == "trash":
		it, err := s.st.TrashByName(user, item)
		if err != nil || it == nil {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case "PROPFIND":
			s.trashCollection(w, r, "", []store.TrashItem{*it}, user)
		case http.MethodGet, http.MethodHead:
			if it.IsDir {
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
				return
			}
			f, err := os.Open(s.fs.TrashPath(user, it.ID))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer f.Close()
			fi, _ := f.Stat()
			w.Header().Set("Content-Type", mimeOf(it.Name))
			http.ServeContent(w, r, it.Name, fi.ModTime(), f)
		case http.MethodDelete:
			if err := s.trashPurge(*it); err != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			empty(http.StatusNoContent)
		case "MOVE":
			if _, err := s.trashRestore(it); err != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			empty(http.StatusCreated)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	default:
		http.NotFound(w, r)
	}
}

// trashCollection writes a multistatus: the collection itself (when href != "")
// followed by the given items.
func (s *Server) trashCollection(w http.ResponseWriter, r *http.Request, href string, items []store.TrashItem, user string) {
	io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	bw := bufio.NewWriterSize(w, 32*1024)
	defer bw.Flush()
	bw.WriteString(xml.Header)
	bw.WriteString(`<d:multistatus xmlns:d="DAV:" xmlns:s="http://sabredav.org/ns" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">`)
	if href != "" {
		bw.WriteString(`<d:response><d:href>` + xesc(href) + `</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
	}
	for i := range items {
		it := &items[i]
		n, err := s.st.NodeByPath(store.TrashOwner(user), strconv.FormatInt(it.ID, 10))
		if err != nil || n == nil {
			continue
		}
		h := "/remote.php/dav/trashbin/" + user + "/trash/" + url.PathEscape(it.ItemName())
		if it.IsDir {
			h += "/"
		}
		s.writeTrashEntry(bw, h, it, n, user)
	}
	bw.WriteString(`</d:multistatus>`)
}

func (s *Server) writeTrashEntry(bw *bufio.Writer, href string, it *store.TrashItem, n *store.Node, user string) {
	bw.WriteString(`<d:response><d:href>` + xesc(href) + `</d:href><d:propstat><d:prop>`)
	bw.WriteString(`<d:getlastmodified>` + time.Unix(n.Mtime, 0).UTC().Format(http.TimeFormat) + `</d:getlastmodified>`)
	if it.IsDir {
		bw.WriteString(`<d:resourcetype><d:collection/></d:resourcetype>`)
	} else {
		bw.WriteString(`<d:resourcetype/><d:getcontentlength>` + strconv.FormatInt(n.Size, 10) + `</d:getcontentlength>`)
		bw.WriteString(`<d:getcontenttype>` + xesc(mimeOf(it.Name)) + `</d:getcontenttype>`)
	}
	bw.WriteString(`<d:getetag>"` + n.ETag + `"</d:getetag>`)
	bw.WriteString(`<oc:id>` + s.fileIDString(n.ID) + `</oc:id>`)
	bw.WriteString(`<oc:fileid>` + strconv.FormatInt(n.ID, 10) + `</oc:fileid>`)
	bw.WriteString(`<oc:size>` + strconv.FormatInt(it.Size, 10) + `</oc:size>`)
	bw.WriteString(`<nc:has-preview>` + strconv.FormatBool(!it.IsDir && s.pv.Supported(it.Name)) + `</nc:has-preview>`)
	bw.WriteString(`<nc:trashbin-filename>` + xesc(it.Name) + `</nc:trashbin-filename>`)
	bw.WriteString(`<nc:trashbin-original-location>` + xesc(it.OrigPath()) + `</nc:trashbin-original-location>`)
	bw.WriteString(`<nc:trashbin-deletion-time>` + strconv.FormatInt(it.DeletedAt, 10) + `</nc:trashbin-deletion-time>`)
	bw.WriteString(`<nc:trashbin-deleted-by-id>` + xesc(user) + `</nc:trashbin-deleted-by-id>`)
	bw.WriteString(`<nc:trashbin-deleted-by-display-name>` + xesc(user) + `</nc:trashbin-deleted-by-display-name>`)
	bw.WriteString(`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
}

// handleTrashPreview serves /index.php/apps/files_trashbin/preview?fileId=..&x=..&y=..
func (s *Server) handleTrashPreview(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authed(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("fileId"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	n, err := s.st.NodeByID(id)
	if err != nil || n == nil || n.IsDir || n.Owner != store.TrashOwner(user) {
		http.NotFound(w, r)
		return
	}
	top, rest, _ := strings.Cut(n.Path, "/")
	tid, err := strconv.ParseInt(top, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	it, err := s.st.TrashByID(tid)
	if err != nil || it == nil || it.Owner != user {
		http.NotFound(w, r)
		return
	}
	name := it.Name
	if rest != "" {
		name = path.Base(rest)
	}
	src := filepath.Join(s.fs.TrashPath(user, tid), filepath.FromSlash(rest))
	s.servePreview(w, r, n, name, src)
}
