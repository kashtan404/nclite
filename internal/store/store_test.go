package store

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func fi(name string, dir bool, size int64) fs.FileInfo {
	mode := fs.FileMode(0o644)
	if dir {
		mode = fs.ModeDir | 0o755
	}
	m := fstest.MapFS{name: &fstest.MapFile{Data: make([]byte, size), Mode: mode, ModTime: time.Unix(1700000000, 0)}}
	i, _ := m.Stat(name)
	return i
}

func open(t *testing.T) *Store {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func etag(t *testing.T, s *Store, p string) string {
	n, err := s.NodeByPath("u", p)
	if err != nil || n == nil {
		t.Fatalf("node %q missing: %v", p, err)
	}
	return n.ETag
}

func TestEtagPropagatesToRoot(t *testing.T) {
	s := open(t)
	for _, d := range []string{"", "a", "a/b"} {
		if _, err := s.SyncPath("u", d, fi("x", true, 0)); err != nil {
			t.Fatal(err)
		}
	}
	rootBefore, aBefore := etag(t, s, ""), etag(t, s, "a")
	if _, err := s.SyncPath("u", "a/b/f.txt", fi("f.txt", false, 10)); err != nil {
		t.Fatal(err)
	}
	if etag(t, s, "") == rootBefore || etag(t, s, "a") == aBefore {
		t.Fatal("ancestor etags did not change")
	}
	// modify: size changes -> file etag and ancestors change again
	f1, root1 := etag(t, s, "a/b/f.txt"), etag(t, s, "")
	s.SyncPath("u", "a/b/f.txt", fi("f.txt", false, 99))
	if etag(t, s, "a/b/f.txt") == f1 || etag(t, s, "") == root1 {
		t.Fatal("modification did not bump etags")
	}
	// identical sync is a no-op
	root2 := etag(t, s, "")
	s.SyncPath("u", "a/b/f.txt", fi("f.txt", false, 99))
	if etag(t, s, "") != root2 {
		t.Fatal("no-op sync changed etags")
	}
}

func TestMoveKeepsIDsAndRewritesSubtree(t *testing.T) {
	s := open(t)
	s.SyncPath("u", "", fi("x", true, 0))
	s.SyncPath("u", "d", fi("x", true, 0))
	n, _ := s.SyncPath("u", "d/f", fi("f", false, 5))
	if err := s.MovePath("u", "d", "e"); err != nil {
		t.Fatal(err)
	}
	if old, _ := s.NodeByPath("u", "d/f"); old != nil {
		t.Fatal("old path still indexed")
	}
	moved, _ := s.NodeByPath("u", "e/f")
	if moved == nil || moved.ID != n.ID || moved.Dir != "e" {
		t.Fatalf("subtree not re-keyed correctly: %+v", moved)
	}
}

func TestDeleteTreeDoesNotTouchSiblingsWithSharedPrefix(t *testing.T) {
	s := open(t)
	s.SyncPath("u", "", fi("x", true, 0))
	for _, p := range []string{"ab", "ab/f", "ab0", "ab-x", "ab.txt"} {
		s.SyncPath("u", p, fi("f", p == "ab", 1))
	}
	s.DeletePath("u", "ab")
	for _, p := range []string{"ab0", "ab-x", "ab.txt"} {
		if n, _ := s.NodeByPath("u", p); n == nil {
			t.Errorf("%q was wrongly deleted", p)
		}
	}
	if n, _ := s.NodeByPath("u", "ab/f"); n != nil {
		t.Error("child not deleted")
	}
}

func TestDirSizeRange(t *testing.T) {
	s := open(t)
	s.SyncPath("u", "", fi("x", true, 0))
	s.SyncPath("u", "a", fi("x", true, 0))
	s.SyncPath("u", "a/f", fi("f", false, 100))
	s.SyncPath("u", "a0", fi("f", false, 7)) // must not count as inside "a"
	if got := s.DirSize("u", "a"); got != 100 {
		t.Fatalf("DirSize(a) = %d, want 100", got)
	}
	if got := s.DirSize("u", ""); got != 107 {
		t.Fatalf("DirSize(root) = %d, want 107", got)
	}
}

func TestTrashRoundTripKeepsIDsAndFavorites(t *testing.T) {
	s := open(t)
	s.SyncPath("u", "", fi("x", true, 0))
	s.SyncPath("u", "d", fi("x", true, 0))
	f, _ := s.SyncPath("u", "d/f.txt", fi("f", false, 5))
	dir, _ := s.NodeByPath("u", "d")
	s.SetFavorite("u", f.ID, true)
	rootBefore := etag(t, s, "")

	it, err := s.TrashAdd("u", "d", "", true, 5)
	if err != nil {
		t.Fatal(err)
	}
	tid := "1"
	if it.ID != 1 {
		t.Fatalf("unexpected trash id %d", it.ID)
	}
	if err := s.RekeyTree("u", "d", TrashOwner("u"), tid); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.NodeByPath("u", "d/f.txt"); n != nil {
		t.Fatal("still visible in the live tree")
	}
	tn, _ := s.NodeByPath(TrashOwner("u"), "1/f.txt")
	if tn == nil || tn.ID != f.ID {
		t.Fatalf("file id not preserved in trash: %+v", tn)
	}
	if etag(t, s, "") == rootBefore {
		t.Fatal("root etag must change when something is trashed")
	}
	if !s.FavoriteSet("u")[f.ID] {
		t.Fatal("favorite lost while in trash")
	}

	// restore under a new name
	if err := s.RekeyTree(TrashOwner("u"), tid, "u", "restored"); err != nil {
		t.Fatal(err)
	}
	back, _ := s.NodeByPath("u", "restored/f.txt")
	top, _ := s.NodeByPath("u", "restored")
	if back == nil || back.ID != f.ID || back.Dir != "restored" || top == nil || top.ID != dir.ID {
		t.Fatalf("restore did not keep ids: %+v %+v", back, top)
	}
	if !s.FavoriteSet("u")[f.ID] {
		t.Fatal("favorite lost by restore")
	}
}

func TestPurgeAndDeleteDropFavorites(t *testing.T) {
	s := open(t)
	s.SyncPath("u", "", fi("x", true, 0))
	a, _ := s.SyncPath("u", "a", fi("a", false, 1))
	b, _ := s.SyncPath("u", "b", fi("b", false, 1))
	s.SetFavorite("u", a.ID, true)
	s.SetFavorite("u", b.ID, true)
	s.DeletePath("u", "a")
	s.PurgeTree("u", "b")
	if len(s.FavoriteSet("u")) != 0 {
		t.Fatal("favorites of deleted nodes must be removed")
	}
}

func TestTrashNamesAreUniqueAndParsable(t *testing.T) {
	s := open(t)
	a, _ := s.TrashAdd("u", "x.d5.txt", "dir", false, 1)
	b, _ := s.TrashAdd("u", "x.d5.txt", "dir", false, 1) // same second, same name
	if a.ItemName() == b.ItemName() {
		t.Fatal("item names must differ")
	}
	got, _ := s.TrashByName("u", b.ItemName())
	if got == nil || got.ID != b.ID || got.OrigPath() != "dir/x.d5.txt" {
		t.Fatalf("lookup by item name failed: %+v", got)
	}
	if g, _ := s.TrashByName("u", "garbage"); g != nil {
		t.Fatal("garbage must not resolve")
	}
}

func TestDirectTokensExpire(t *testing.T) {
	s := open(t)
	s.AddDirect("live", "u", 7, time.Now().Add(time.Hour))
	s.AddDirect("dead", "u", 7, time.Now().Add(-time.Hour))
	if _, id, ok := s.GetDirect("live"); !ok || id != 7 {
		t.Fatal("live token rejected")
	}
	if _, _, ok := s.GetDirect("dead"); ok {
		t.Fatal("expired token accepted")
	}
}

func TestUserStatsAndMeta(t *testing.T) {
	s := open(t)
	s.SyncPath("u", "", fi("x", true, 0))
	s.SyncPath("u", "d", fi("x", true, 0))
	s.SyncPath("u", "d/a", fi("a", false, 100))
	s.SyncPath("u", "b", fi("b", false, 23))
	s.SyncPath("other", "z", fi("z", false, 999))
	if f, d, b := s.UserStats("u"); f != 2 || d != 1 || b != 123 {
		t.Fatalf("UserStats = %d files, %d dirs, %d bytes", f, d, b)
	}
	s.TrashAdd("u", "x", "", false, 40)
	s.TrashAdd("u", "y", "", false, 2)
	if n, b := s.TrashStats("u"); n != 2 || b != 42 {
		t.Fatalf("TrashStats = %d, %d", n, b)
	}
	if s.GetMeta("k") != "" {
		t.Fatal("unset meta must be empty")
	}
	s.SetMeta("k", "1")
	s.SetMeta("k", "2")
	if s.GetMeta("k") != "2" {
		t.Fatal("SetMeta must overwrite")
	}
}

func TestReconcileRejectsStaleSnapshotOfMovedDir(t *testing.T) {
	s := open(t)
	s.SyncPath("u", "", fi("x", true, 0))
	s.SyncPath("u", "X", fi("x", true, 0))
	f, _ := s.SyncPath("u", "X/a", fi("a", false, 5))

	// a slow listing of X took its snapshot of the disk ...
	snapshot := []fs.FileInfo{fi("a", false, 5), fi("b", false, 7)}
	// ... then X was renamed to Y (disk + index)
	if err := s.MovePath("u", "X", "Y"); err != nil {
		t.Fatal(err)
	}
	// applying the old snapshot must change nothing (this used to index X/a and X/b a second time)
	onDisk := func(rel string) bool { return !strings.HasPrefix(rel, "X") }
	if _, err := s.ReconcileDir("u", "X", snapshot, onDisk); err != ErrStale {
		t.Fatalf("want ErrStale, got %v", err)
	}
	if n, _ := s.NodeByPath("u", "X/a"); n != nil {
		t.Fatal("phantom row created under the old path")
	}
	moved, _ := s.NodeByPath("u", "Y/a")
	if moved == nil || moved.ID != f.ID {
		t.Fatal("the real record must be untouched")
	}
	if files, _, _ := s.UserStats("u"); files != 1 {
		t.Fatalf("files counted: %d, want 1", files)
	}
}

func TestReconcileKeepsFreshFileAndSkipsVanishedOne(t *testing.T) {
	s := open(t)
	s.SyncPath("u", "", fi("x", true, 0))
	s.SyncPath("u", "d", fi("x", true, 0))
	fresh, _ := s.SyncPath("u", "d/fresh", fi("fresh", false, 3)) // uploaded after the snapshot below
	existing := map[string]bool{"d": true, "d/fresh": true}       // d/gone does not exist any more
	onDisk := func(rel string) bool { return existing[rel] }

	snapshot := []fs.FileInfo{fi("gone", false, 1)} // taken before 'fresh' arrived, and 'gone' was deleted since
	nodes, err := s.ReconcileDir("u", "d", snapshot, onDisk)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.NodeByPath("u", "d/fresh"); got == nil || got.ID != fresh.ID {
		t.Fatal("a file created after the snapshot must keep its record and id")
	}
	if got, _ := s.NodeByPath("u", "d/gone"); got != nil {
		t.Fatal("a file that no longer exists must not be indexed")
	}
	for _, n := range nodes {
		if n.Path == "d/gone" {
			t.Fatal("vanished file returned in the listing")
		}
	}
}

func TestJoinNeverAddsTrailingSlash(t *testing.T) {
	for _, c := range [][3]string{{"", "a", "a"}, {"a", "b", "a/b"}, {"a", "", "a"}, {"", "", ""}} {
		if got := Join(c[0], c[1]); got != c[2] {
			t.Errorf("Join(%q,%q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestIndexRefusesAliasPathsAndPurgeCleansOldOnes(t *testing.T) {
	s := open(t)
	s.SyncPath("u", "", fi("x", true, 0))
	s.SyncPath("u", "Photos", fi("x", true, 0))
	good, _ := s.SyncPath("u", "Photos/a.jpg", fi("a", false, 10))
	for _, bad := range []string{"Photos/", "Photos//a.jpg", "/Photos"} {
		if _, err := s.SyncPath("u", bad, fi("x", true, 0)); err == nil {
			t.Errorf("SyncPath(%q) must be refused", bad)
		}
	}
	if _, err := s.ReconcileDir("u", "Photos/", nil, nil); err == nil {
		t.Error("ReconcileDir on a non-canonical dir must be refused")
	}

	// damage as an older version left it: a second copy under "Photos/"
	s.db.Exec(`INSERT INTO nodes(owner,path,dir,is_dir,size,mtime,etag) VALUES('u','Photos/','Photos',1,0,1,'e')`)
	s.db.Exec(`INSERT INTO nodes(owner,path,dir,is_dir,size,mtime,etag) VALUES('u','Photos//a.jpg','Photos/',0,10,1,'e')`)
	var dup int64
	s.db.QueryRow(`SELECT id FROM nodes WHERE path='Photos//a.jpg'`).Scan(&dup)
	s.SetFavorite("u", dup, true)
	if f, _, _ := s.UserStats("u"); f != 2 {
		t.Fatalf("setup: %d files", f)
	}
	n, err := s.PurgeBadPaths()
	if err != nil || n != 2 {
		t.Fatalf("purged %d rows, err %v", n, err)
	}
	if f, d, _ := s.UserStats("u"); f != 1 || d != 1 {
		t.Fatalf("after purge: %d files %d dirs, want 1/1", f, d)
	}
	if got, _ := s.NodeByPath("u", "Photos/a.jpg"); got == nil || got.ID != good.ID {
		t.Fatal("the real row must be untouched")
	}
	if len(s.FavoriteSet("u")) != 0 {
		t.Fatal("favorites of purged duplicates must go too")
	}
}
