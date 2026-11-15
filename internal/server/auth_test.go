package server

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPBKDF2Vectors(t *testing.T) {
	// reference values from Python hashlib.pbkdf2_hmac('sha256', ...)
	cases := []struct {
		iter, n int
		want    string
	}{
		{1, 32, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{4096, 40, "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134af7ad98c1b458ce3f"},
	}
	for _, c := range cases {
		if got := hex.EncodeToString(pbkdf2([]byte("password"), []byte("salt"), c.iter, c.n)); got != c.want {
			t.Errorf("iter=%d: got %s", c.iter, got)
		}
	}
}

func TestPasswordRoundTrip(t *testing.T) {
	h := HashPassword("correct horse")
	if !CheckPassword(h, "correct horse") || CheckPassword(h, "wrong") || CheckPassword("garbage", "x") {
		t.Fatal("password check misbehaves")
	}
}

func TestUniqueName(t *testing.T) {
	dir := t.TempDir()
	touch := func(n string) { os.WriteFile(filepath.Join(dir, n), nil, 0o644) }
	if got := uniqueName(dir, "a.txt"); got != "a.txt" {
		t.Fatalf("free name changed: %s", got)
	}
	touch("a.txt")
	if got := uniqueName(dir, "a.txt"); got != "a (restored).txt" {
		t.Fatalf("got %s", got)
	}
	touch("a (restored).txt")
	if got := uniqueName(dir, "a.txt"); got != "a (restored 2).txt" {
		t.Fatalf("got %s", got)
	}
}

func TestParseSizes(t *testing.T) {
	got, err := parseSizes("512, 1024x768 256c 512")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1024x1024", "512x512", "256x256c"} // sides snap to buckets, duplicates dropped, biggest first
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("size %d: got %s want %s", i, got[i], want[i])
		}
	}
	for _, bad := range []string{"", "abc", "5", "99999", "1 2 3 4 5 6 7 8"} {
		if _, err := parseSizes(bad); err == nil {
			t.Errorf("parseSizes(%q) should fail", bad)
		}
	}
}

func TestParseSizesOrder(t *testing.T) {
	got, err := parseSizes("256c 256 1024 512c 512")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, z := range got {
		names = append(names, z.String())
	}
	want := "1024x1024 512x512 512x512c 256x256 256x256c" // fit before cover at the same size
	if strings.Join(names, " ") != want {
		t.Fatalf("got %v", names)
	}
}
