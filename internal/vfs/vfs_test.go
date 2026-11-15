package vfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"nclite/internal/store"
)

func TestCleanRel(t *testing.T) {
	ok := map[string]string{"": "", "/": "", "a": "a", "/a/b/": "a/b", "a//b/./c": "a/b/c", "Фото/лето": "Фото/лето"}
	for in, want := range ok {
		if got, err := CleanRel(in); err != nil || got != want {
			t.Errorf("CleanRel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"..", "a/../b", "../etc", "a/..", "a\x00b"} {
		if _, err := CleanRel(in); err == nil {
			t.Errorf("CleanRel(%q) should fail", in)
		}
	}
}

func TestShareMountNames(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, sh := range [][3]string{
		{"bob", "Photos", "alice"},
		{"carol", "Photos", "alice"},   // same folder name from another owner
		{"bob", "old/Photos", "alice"}, // same owner, same folder name, other place
		{"bob", "Docs", "alice"},
		{"bob", "Photos", "dave"}, // other recipient: independent numbering
	} {
		if err := st.AddShare(sh[0], sh[1], sh[2], false); err != nil {
			t.Fatal(err)
		}
	}
	v, err := New(t.TempDir(), st)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range v.Mounts("alice") {
		got = append(got, m.Name)
	}
	want := []string{"Photos (bob)", "Photos (carol)", "Photos (bob 2)", "Docs (bob)"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mount %d: got %q want %q", i, got[i], want[i])
		}
	}
	if m := v.Mounts("dave"); len(m) != 1 || m[0].Name != "Photos (bob)" {
		t.Fatalf("dave: %+v", m)
	}
	// and the name resolves to the owner's real path, including paths below it
	if tg := v.Resolve("alice", "Photos (carol)/2024/x.jpg"); tg.Owner != "carol" || tg.Rel != "Photos/2024/x.jpg" || !tg.InMount {
		t.Fatalf("resolve: %+v", tg)
	}
}

func TestUnmountedDiskDoesNotWipeIndex(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	data := t.TempDir()
	v, _ := New(data, st)
	if err := v.EnsureUser("bob"); err != nil { // a brand-new account: folders are created
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(data, "bob", "files", "a.txt"), []byte("x"), 0o644)
	if _, err := v.ListDir("bob", ""); err != nil {
		t.Fatal(err)
	}
	if files, _, _ := st.UserStats("bob"); files != 1 {
		t.Fatalf("indexed %d files", files)
	}

	os.RemoveAll(filepath.Join(data, "bob")) // the disk disappears: mount point is empty again
	if _, err := v.ListDir("bob", ""); !errors.Is(err, ErrStorageMissing) {
		t.Fatalf("want ErrStorageMissing, got %v", err)
	}
	if files, _, _ := st.UserStats("bob"); files != 1 {
		t.Fatal("index must survive a missing disk")
	}
	if _, err := os.Stat(filepath.Join(data, "bob")); err == nil {
		t.Fatal("must not create an empty folder in place of the missing disk")
	}
	// a Walk (the periodic scan) must not wipe anything either
	v.Walk(context.Background(), "bob", "", nil)
	if files, _, _ := st.UserStats("bob"); files != 1 {
		t.Fatal("scan wiped the index")
	}
}

func TestDetectCaseInsensitiveOnNormalDisk(t *testing.T) {
	// the sandbox/test disk is a normal case-sensitive one; the probe must not leave files behind
	dir := t.TempDir()
	if detectCaseInsensitive(dir) {
		t.Skip("running on a case-insensitive disk")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("probe left %d files behind", len(ents))
	}
}

// Opening the root of a shared folder used to resolve to "Photos/" (trailing slash):
// a second spelling of the same folder, indexed again with all its files.
func TestMountRootResolvesToCanonicalPath(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.AddShare("alice", "Photos", "bob", false)
	st.AddShare("alice", "Family/Trips", "bob", true)
	v, _ := New(t.TempDir(), st)

	for _, c := range []struct{ in, wantOwner, wantRel string }{
		{"Photos (alice)", "alice", "Photos"},
		{"Photos (alice)/", "alice", "Photos"},
		{"Photos (alice)/2024", "alice", "Photos/2024"},
		{"Trips (alice)", "alice", "Family/Trips"},
	} {
		in, _ := CleanRel(c.in)
		tg := v.Resolve("bob", in)
		if tg.Owner != c.wantOwner || tg.Rel != c.wantRel {
			t.Errorf("Resolve(%q) = %s:%q, want %s:%q", c.in, tg.Owner, tg.Rel, c.wantOwner, c.wantRel)
		}
	}
	if tg := v.Resolve("bob", "Photos (alice)"); !tg.MountRoot || !tg.InMount {
		t.Errorf("mount root flags lost: %+v", tg)
	}
}

func TestCleanIdx(t *testing.T) {
	for in, want := range map[string]string{"": "", "a": "a", "a/b": "a/b", "a/": "a", "/a": "a", "a//b": "a/b", "a/./b/": "a/b"} {
		if got := cleanIdx(in); got != want {
			t.Errorf("cleanIdx(%q) = %q, want %q", in, got, want)
		}
	}
}
