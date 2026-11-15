// Package vfs maps user-visible paths to files on disk, including folders
// shared by other users (which appear as mount points in the recipient's root).
package vfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"nclite/internal/store"
)

type VFS struct {
	dataDir string
	st      *store.Store
	// treeMu orders structural changes against directory reconciliation: moves, renames,
	// trash and restore take the write side, listings the read side, so a listing can
	// never apply a snapshot of the disk that a rename has already made outdated.
	treeMu sync.RWMutex
	ci     bool // the data disk ignores letter case (FAT, exFAT, ...): "Photos" and "photos" are one folder

	mu     sync.RWMutex
	mounts map[string][]Mount // by recipient
}

type Mount struct {
	Name     string // name shown in the recipient's root
	Owner    string
	Path     string // path inside the owner's tree
	ReadOnly bool
}

// Target is a resolved user-visible path.
type Target struct {
	Owner     string
	Rel       string // path inside Owner's tree
	ReadOnly  bool
	MountRoot bool // the path is the mount point itself (cannot be renamed/deleted by the recipient)
	InMount   bool
}

func New(dataDir string, st *store.Store) (*VFS, error) {
	v := &VFS{dataDir: dataDir, st: st, ci: detectCaseInsensitive(dataDir)}
	if v.ci {
		slog.Info("data disk ignores letter case: request paths are matched to the spelling on disk", "dir", dataDir)
	}
	return v, v.ReloadShares()
}

// cleanIdx drops empty and "." segments, so "Photos/" or "a//b" can never become an
// index key of their own.
func cleanIdx(rel string) string {
	if rel == "" || (rel[0] != '/' && rel[len(rel)-1] != '/' && !strings.Contains(rel, "//")) {
		return rel
	}
	var parts []string
	for _, seg := range strings.Split(rel, "/") {
		if seg != "" && seg != "." {
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, "/")
}

// CaseInsensitive reports whether the data disk treats "A" and "a" as the same name.
func (v *VFS) CaseInsensitive() bool { return v.ci }

// detectCaseInsensitive asks the disk itself: create a file with mixed-case letters
// and see whether its lower-case spelling resolves to it.
func detectCaseInsensitive(dir string) bool {
	name := fmt.Sprintf(".NcLite-CaseProbe-%d", os.Getpid())
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		return false // cannot tell; assume a normal case-sensitive disk
	}
	f.Close()
	defer os.Remove(p)
	_, err = os.Lstat(filepath.Join(dir, strings.ToLower(name)))
	return err == nil
}

// Canon returns rel with every existing component spelled exactly as it is on
// disk. On a case-insensitive disk a client may reach a folder under another letter
// case; if that spelling were indexed, the whole folder would be counted twice.
// Components that do not exist yet (a file about to be created) are kept as given.
// On normal disks, and for paths that are already indexed, this costs nothing.
func (v *VFS) Canon(owner, rel string) string {
	rel = cleanIdx(rel)
	if !v.ci || rel == "" {
		return rel
	}
	if n, _ := v.st.NodeByPath(owner, rel); n != nil {
		return rel // indexed as written, hence already the real spelling
	}
	segs := strings.Split(rel, "/")
	cur := ""
	for i, seg := range segs {
		real, ok := v.realName(owner, cur, seg)
		if !ok {
			return store.Join(cur, strings.Join(segs[i:], "/"))
		}
		cur = store.Join(cur, real)
	}
	return cur
}

// CanonParent canonicalizes everything but the last component, which keeps the
// spelling the client chose (the name for a file that is being created or renamed).
func (v *VFS) CanonParent(owner, rel string) string {
	if !v.ci || rel == "" {
		return rel
	}
	return store.Join(v.Canon(owner, store.ParentOf(rel)), path.Base(rel))
}

// realName finds how the entry called seg (in any letter case) is spelled in dir.
func (v *VFS) realName(owner, dir, seg string) (string, bool) {
	if n, _ := v.st.NodeByPath(owner, store.Join(dir, seg)); n != nil {
		return seg, true
	}
	if p := v.st.PathNoCase(owner, dir, seg); p != "" {
		return path.Base(p), true
	}
	if _, err := os.Lstat(v.Real(owner, store.Join(dir, seg))); err != nil {
		return seg, false // really not there
	}
	// exists on disk but not indexed yet: read the directory to learn the spelling
	ents, err := os.ReadDir(v.Real(owner, dir))
	if err != nil {
		return seg, false
	}
	for _, e := range ents {
		if e.Name() == seg {
			return seg, true
		}
	}
	for _, e := range ents {
		if strings.EqualFold(e.Name(), seg) {
			return e.Name(), true
		}
	}
	return seg, false
}

// ReloadShares rebuilds the mount table from the database.
func (v *VFS) ReloadShares() error {
	shares, err := v.st.ListShares()
	if err != nil {
		return err
	}
	m := map[string][]Mount{}
	used := map[string]map[string]bool{}
	for _, sh := range shares {
		if used[sh.Target] == nil {
			used[sh.Target] = map[string]bool{}
		}
		// Always "<folder> (<owner>)", so a shared folder can never collide with the
		// recipient's own folders or with same-named folders shared by other people.
		// The same owner sharing two folders with one name gets a counter.
		base := path.Base(sh.Path)
		name := fmt.Sprintf("%s (%s)", base, sh.Owner)
		for i := 2; used[sh.Target][name]; i++ {
			name = fmt.Sprintf("%s (%s %d)", base, sh.Owner, i)
		}
		used[sh.Target][name] = true
		m[sh.Target] = append(m[sh.Target], Mount{Name: name, Owner: sh.Owner, Path: v.Canon(sh.Owner, sh.Path), ReadOnly: sh.ReadOnly})
	}
	v.mu.Lock()
	v.mounts = m
	v.mu.Unlock()
	return nil
}

func (v *VFS) Mounts(user string) []Mount {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.mounts[user]
}

var ErrBadPath = errors.New("bad path")

// CleanRel validates a user-supplied path and returns it without leading or
// trailing slashes ("" is the root).
func CleanRel(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", ErrBadPath
	}
	var parts []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			return "", ErrBadPath
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, "/"), nil
}

func (v *VFS) Resolve(user, rel string) Target {
	if rel != "" {
		first, rest, _ := strings.Cut(rel, "/")
		for _, m := range v.Mounts(user) {
			if m.Name == first {
				return Target{Owner: m.Owner, Rel: store.Join(m.Path, rest), ReadOnly: m.ReadOnly,
					MountRoot: rest == "", InMount: true}
			}
		}
	}
	return Target{Owner: user, Rel: rel}
}

// CanAccess reports whether user may reach node n (by file id) and whether read-only.
func (v *VFS) CanAccess(user string, n *store.Node) (readOnly, ok bool) {
	if n.Owner == user {
		return false, true
	}
	for _, m := range v.Mounts(user) {
		if m.Owner == n.Owner && (n.Path == m.Path || strings.HasPrefix(n.Path, m.Path+"/")) {
			return m.ReadOnly, true
		}
	}
	return false, false
}

func (v *VFS) FilesRoot(owner string) string { return filepath.Join(v.dataDir, owner, "files") }
func (v *VFS) TmpDir(owner string) string    { return filepath.Join(v.dataDir, owner, "tmp") }

// TrashPath is where a trashed item lives on disk. It sits next to files/ and tmp/
// so moving into the trash is a same-filesystem rename (instant, no copying).
func (v *VFS) TrashPath(owner string, id int64) string {
	return filepath.Join(v.dataDir, owner, "trash", strconv.FormatInt(id, 10))
}

// VisiblePath maps an indexed node back to the path the given user sees it under
// (their own tree, or below a mount point of a folder shared with them).
func (v *VFS) VisiblePath(user string, n *store.Node) (rel string, readOnly, ok bool) {
	if n.Owner == user {
		return n.Path, false, true
	}
	for _, m := range v.Mounts(user) {
		if m.Owner == n.Owner && (n.Path == m.Path || strings.HasPrefix(n.Path, m.Path+"/")) {
			return m.Name + strings.TrimPrefix(n.Path, m.Path), m.ReadOnly, true
		}
	}
	return "", false, false
}

func (v *VFS) Real(owner, rel string) string {
	return filepath.Join(v.FilesRoot(owner), filepath.FromSlash(rel))
}

// ErrStorageMissing means the index knows files of this user but their folder is gone
// from the data disk: the disk is unplugged or not mounted yet. Creating an empty
// folder there would make every scan conclude that all files were deleted.
var ErrStorageMissing = errors.New("storage unavailable: the data disk looks unmounted")

func (v *VFS) EnsureUser(owner string) error {
	if _, err := os.Stat(filepath.Join(v.dataDir, owner)); errors.Is(err, fs.ErrNotExist) {
		if files, dirs, _ := v.st.UserStats(owner); files+dirs > 0 {
			return ErrStorageMissing
		}
	}
	for _, d := range []string{v.FilesRoot(owner), v.TmpDir(owner), filepath.Join(v.dataDir, owner, "trash")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// NodeFor stats rel on disk and returns its index node, (re)syncing it only
// when the on-disk state differs. Returns fs.ErrNotExist if missing.
func (v *VFS) NodeFor(owner, rel string) (*store.Node, error) {
	rel = v.Canon(owner, rel) // also normalises the path
	if rel == "" {
		if err := v.EnsureUser(owner); err != nil {
			return nil, err
		}
	}
	fi, err := os.Stat(v.Real(owner, rel))
	if err != nil {
		return nil, err
	}
	n, err := v.st.NodeByPath(owner, rel)
	if err != nil {
		return nil, err
	}
	if n != nil && n.IsDir == fi.IsDir() && (fi.IsDir() || (n.Size == fi.Size() && n.Mtime == fi.ModTime().Unix())) {
		return n, nil
	}
	return v.st.SyncPath(owner, rel, fi)
}

// Structural runs fn (a rename/move on disk plus the matching index update) so that
// no directory listing is applied to the index while it is half done.
func (v *VFS) Structural(fn func() error) error {
	v.treeMu.Lock()
	defer v.treeMu.Unlock()
	return fn()
}

func (v *VFS) onDisk(owner string) func(string) bool {
	return func(rel string) bool {
		_, err := os.Lstat(v.Real(owner, rel))
		return err == nil
	}
}

// ListDir reconciles one directory with the index and returns its children.
func (v *VFS) ListDir(owner, rel string) ([]*store.Node, error) {
	rel = v.Canon(owner, rel)
	for attempt := 0; ; attempt++ {
		if _, err := v.NodeFor(owner, rel); err != nil { // makes sure the dir node exists
			return nil, err
		}
		nodes, err := v.listOnce(owner, rel)
		if errors.Is(err, store.ErrStale) {
			if attempt < 2 {
				continue // moved or removed while we were reading it: look again
			}
			return nil, fs.ErrNotExist
		}
		return nodes, err
	}
}

func (v *VFS) listOnce(owner, rel string) ([]*store.Node, error) {
	v.treeMu.RLock()
	defer v.treeMu.RUnlock()
	ents, err := os.ReadDir(v.Real(owner, rel))
	if err != nil {
		return nil, err
	}
	infos := make([]fs.FileInfo, 0, len(ents))
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil || !(fi.IsDir() || fi.Mode().IsRegular()) {
			continue
		}
		infos = append(infos, fi)
	}
	nodes, err := v.st.ReconcileDir(owner, rel, infos, v.onDisk(owner))
	if err != nil {
		return nil, err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Path < nodes[j].Path })
	return nodes, nil
}

// Walk reconciles every directory below rel with the index (so whatever is found
// gets indexed) and calls visit for each directory with its children. visit may be
// nil; returning false stops the walk. It pauses briefly between directories so a
// long scan never competes with real traffic. Unreadable subdirectories are
// skipped; an error is returned only when the starting point itself is unusable.
func (v *VFS) Walk(ctx context.Context, owner, rel string, visit func(dir string, nodes []*store.Node) bool) error {
	nodes, err := v.ListDir(owner, rel)
	if err != nil {
		return err
	}
	if visit != nil && !visit(rel, nodes) {
		return context.Canceled
	}
	for _, n := range nodes {
		if !n.IsDir {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
		if err := v.Walk(ctx, owner, n.Path, visit); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

// Rescan walks user trees to pick up files added, changed or removed outside the
// server (see Walk). With onlyNew it skips users that have been scanned before, so
// a freshly connected folder gets indexed without re-walking everyone.
func (v *VFS) Rescan(ctx context.Context, onlyNew bool) {
	users, err := v.st.ListUsers()
	if err != nil {
		return
	}
	for _, u := range users {
		if ctx.Err() != nil {
			return
		}
		if onlyNew && v.st.GetMeta("lastscan:"+u.Name) != "" {
			continue
		}
		if v.Walk(ctx, u.Name, "", nil) == nil && ctx.Err() == nil {
			v.st.SetMeta("lastscan:"+u.Name, strconv.FormatInt(time.Now().Unix(), 10))
		}
	}
}
