package server

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"

	"nclite/internal/store"
	"nclite/internal/vfs"
)

// File operations shared by the WebDAV handlers and the web UI. They work on the
// paths a user sees (own files, plus shared folders under their mount names) and
// return an error carrying the HTTP status that fits it, so each front end only
// has to translate it.

type opErr struct {
	code int
	msg  string
}

func (e *opErr) Error() string { return e.msg }

func opf(code int, msg string) *opErr { return &opErr{code, msg} }

// httpStatus maps any error from the file layer to a status code and a short message.
func httpStatus(err error) (int, string) {
	var oe *opErr
	switch {
	case errors.As(err, &oe):
		return oe.code, oe.msg
	case errors.Is(err, vfs.ErrStorageMissing):
		return http.StatusServiceUnavailable, "Storage unavailable"
	case errors.Is(err, fs.ErrNotExist):
		return http.StatusNotFound, "Not Found"
	case errors.Is(err, fs.ErrPermission):
		return http.StatusForbidden, "Forbidden"
	}
	return http.StatusInternalServerError, "Internal Server Error"
}

func (s *Server) parentIsDir(owner, rel string) bool {
	fi, err := os.Stat(s.fs.Real(owner, store.ParentOf(rel)))
	return err == nil && fi.IsDir()
}

// doMoveCopy moves or copies srcRel to dstRel (both as the user sees them). With
// overwrite=false an existing destination is an error (412); with true it is sent to
// the trash first. Returns the destination node and whether something was replaced.
//
// Source and destination may belong to different accounts (a folder shared with you
// is the other person's storage). Then the data really is copied between the two
// storages; a "move" is that copy followed by sending the original to the trash.
func (s *Server) doMoveCopy(user, srcRel, dstRel string, move, overwrite bool) (*store.Node, bool, error) {
	t := s.fs.Resolve(user, srcRel)
	t.Rel = s.fs.Canon(t.Owner, t.Rel)
	t2 := s.fs.Resolve(user, dstRel)
	// destination: real spelling for the parents, the leaf keeps what the client chose
	t2.Rel = s.fs.CanonParent(t2.Owner, t2.Rel)
	cross := t2.Owner != t.Owner
	// on a case-insensitive disk "a.txt" and "A.TXT" are the same object
	sameObject := !cross && s.fs.CaseInsensitive() && strings.EqualFold(t2.Rel, t.Rel)
	switch {
	case sameObject && !move: // copying a file onto itself under another letter case
		return nil, false, opf(http.StatusForbidden, "Source and destination are the same")
	case t.Rel == "" || t2.Rel == "":
		return nil, false, opf(http.StatusForbidden, "A top-level folder cannot be used here")
	case t2.ReadOnly:
		return nil, false, opf(http.StatusForbidden, "The destination is read-only")
	case t2.MountRoot:
		return nil, false, opf(http.StatusForbidden, "Put the item inside the shared folder, not in its place")
	case move && t.ReadOnly:
		return nil, false, opf(http.StatusForbidden, "The source is read-only: copy it instead of moving")
	case move && t.MountRoot:
		return nil, false, opf(http.StatusForbidden, "A shared folder itself cannot be moved or renamed")
	case !cross && t2.Rel == t.Rel:
		return nil, false, opf(http.StatusForbidden, "Source and destination are the same")
	case !cross && strings.HasPrefix(t2.Rel+"/", t.Rel+"/"):
		return nil, false, opf(http.StatusForbidden, "A folder cannot be moved or copied into itself")
	}
	src, dst := s.fs.Real(t.Owner, t.Rel), s.fs.Real(t2.Owner, t2.Rel)
	if _, err := os.Stat(src); err != nil {
		return nil, false, err
	}
	if !s.parentIsDir(t2.Owner, t2.Rel) {
		return nil, false, opf(http.StatusConflict, "Destination folder does not exist")
	}
	_, statErr := os.Stat(dst)
	// a rename that only changes letter case "finds" the file itself at the destination
	existed := statErr == nil && !sameObject
	if existed {
		if !overwrite {
			return nil, false, opf(http.StatusPreconditionFailed, "Destination already exists")
		}
		if err := s.removeToTrash(t2.Owner, s.fs.Canon(t2.Owner, t2.Rel)); err != nil {
			return nil, false, err
		}
	}
	switch {
	case cross:
		if err := copyTree(src, dst); err != nil {
			os.RemoveAll(dst) // never leave half a copy in someone's storage
			return nil, false, err
		}
		n, err := s.indexCopy(t2.Owner, t2.Rel)
		if err == nil && move {
			err = s.removeToTrash(t.Owner, t.Rel)
		}
		return n, existed, err
	case move:
		err := s.fs.Structural(func() error {
			if err := os.Rename(src, dst); err != nil {
				return err
			}
			return s.st.MovePath(t.Owner, t.Rel, t2.Rel)
		})
		if err != nil {
			return nil, false, err
		}
	default:
		if err := copyTree(src, dst); err != nil {
			return nil, false, err
		}
		n, err := s.indexCopy(t2.Owner, t2.Rel)
		return n, existed, err
	}
	n, err := s.fs.NodeFor(t2.Owner, t2.Rel)
	return n, existed, err
}

// indexCopy indexes what a copy just wrote. For a folder this walks the new tree right
// away (its files are hot in the disk cache), so the destination account's used space and
// file count are right immediately instead of after the next scan.
func (s *Server) indexCopy(owner, rel string) (*store.Node, error) {
	n, err := s.fs.NodeFor(owner, rel)
	if err == nil && n.IsDir {
		s.fs.Walk(context.Background(), owner, rel, nil)
		n, err = s.fs.NodeFor(owner, rel)
	}
	return n, err
}

// doDelete sends a file or folder to the trash. It can never remove a user's root
// folder or the root of a shared folder.
func (s *Server) doDelete(user, rel string) error {
	t := s.fs.Resolve(user, rel)
	t.Rel = s.fs.Canon(t.Owner, t.Rel)
	if t.ReadOnly || t.MountRoot || t.Rel == "" {
		return opf(http.StatusForbidden, "Forbidden")
	}
	if _, err := os.Lstat(s.fs.Real(t.Owner, t.Rel)); err != nil {
		return err
	}
	return s.removeToTrash(t.Owner, t.Rel)
}

// doMkdir creates one folder; its parent must exist.
func (s *Server) doMkdir(user, rel string) (*store.Node, error) {
	t := s.fs.Resolve(user, rel)
	t.Rel = s.fs.Canon(t.Owner, t.Rel)
	if t.ReadOnly {
		return nil, opf(http.StatusForbidden, "Forbidden")
	}
	if t.Rel == "" {
		return nil, opf(http.StatusMethodNotAllowed, "Method Not Allowed")
	}
	if !s.parentIsDir(t.Owner, t.Rel) {
		return nil, opf(http.StatusConflict, "Parent folder does not exist")
	}
	real := s.fs.Real(t.Owner, t.Rel)
	var n *store.Node
	err := s.fs.Structural(func() error {
		if err := os.Mkdir(real, 0o755); err != nil {
			return err
		}
		fi, err := os.Stat(real)
		if err != nil {
			return err
		}
		n, err = s.st.SyncPath(t.Owner, t.Rel, fi)
		return err
	})
	if errors.Is(err, fs.ErrExist) {
		return nil, opf(http.StatusMethodNotAllowed, "Already exists")
	}
	return n, err
}

// validName rejects names that cannot be a single file or folder name.
func validName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 || path.Base(name) != name {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}
