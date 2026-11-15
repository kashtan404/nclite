package preview

import (
	"nclite/internal/config"
	"os"
	"path/filepath"
	"testing"
)

func TestFindHeifOutput(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, n int) {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := filepath.Join(dir, "i-abc")

	if _, ok := findHeifOutput(base); ok {
		t.Fatal("nothing written yet")
	}
	write("i-abc.jpg", 0) // an empty file is not a result
	if _, ok := findHeifOutput(base); ok {
		t.Fatal("empty output must not count")
	}
	write("i-abc.jpg", 10)
	if got, ok := findHeifOutput(base); !ok || got != base+".jpg" {
		t.Fatalf("plain output not found: %q %v", got, ok)
	}

	// several images in one file: <base>-1.jpg, <base>-2.jpg; first wins, extras removed
	os.Remove(base + ".jpg")
	write("i-abc-1.jpg", 10)
	write("i-abc-2.jpg", 10)
	got, ok := findHeifOutput(base)
	if !ok || got != base+"-1.jpg" {
		t.Fatalf("multi-image output: %q %v", got, ok)
	}
	if _, err := os.Stat(base + "-2.jpg"); err == nil {
		t.Fatal("extra image should be cleaned up")
	}
}

func TestFilterOrientation(t *testing.T) {
	// EXIF 6 (rotate 90 CW) must rotate before scaling so w/h refer to the displayed image
	got := filter(512, 512, false, 6)
	want := "transpose=1,scale='min(512,iw)':'min(512,ih)':force_original_aspect_ratio=decrease"
	if got != want {
		t.Fatalf("got %s", got)
	}
	if filter(100, 100, true, 1) != "scale=100:100:force_original_aspect_ratio=increase,crop=100:100" {
		t.Fatal("cover filter wrong")
	}
}

// fakeJPEG writes just enough of a JPEG (SOI + SOF0) for jpegInfo to read its size.
func fakeJPEG(t *testing.T, path string, w, h int) {
	b := []byte{0xFF, 0xD8, 0xFF, 0xC0, 0x00, 0x11, 0x08, byte(h >> 8), byte(h), byte(w >> 8), byte(w), 0x03}
	b = append(b, make([]byte, 30)...)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindDonor(t *testing.T) {
	dir := t.TempDir()
	// name = <box>-<mode>-<etag>.jpg ; the numbers are the REAL pixels of a 4:3 photo
	fakeJPEG(t, filepath.Join(dir, "1024x1024-fit-E.jpg"), 1024, 768)
	fakeJPEG(t, filepath.Join(dir, "2048x2048-fit-E.jpg"), 2048, 1536)
	fakeJPEG(t, filepath.Join(dir, "512x512-cover-E.jpg"), 512, 512)
	fakeJPEG(t, filepath.Join(dir, "1024x1024-cover-E.jpg"), 1024, 1024)
	fakeJPEG(t, filepath.Join(dir, "256x256-fit-OLD.jpg"), 256, 192) // other file version: never a donor
	fakeJPEG(t, filepath.Join(dir, "1024x1024-fit-E.jpg.part.jpg"), 1, 1)
	m := &Manager{}

	cases := []struct {
		name  string
		w, h  int
		cover bool
		want  string // "" = must use the original
	}{
		{"smaller fit comes from the smallest bigger fit", 512, 512, false, "1024x1024-fit-E.jpg"},
		{"much smaller fit", 256, 256, false, "1024x1024-fit-E.jpg"},
		{"largest fit has no donor", 2048, 2048, false, ""},
		{"non-square fit box still fits inside the donor box", 512, 256, false, "1024x1024-fit-E.jpg"},
		{"a cropped preview is never a donor for fit", 600, 600, false, "1024x1024-fit-E.jpg"},
		{"cover from cover of the same aspect, smallest wins", 256, 256, true, "512x512-cover-E.jpg"},
		{"cover of another aspect cannot use a square crop", 1024, 512, true, "1024x1024-fit-E.jpg"},
		{"cover needs real pixels: 768 high is not enough for 1024", 1024, 1024, true, "2048x2048-fit-E.jpg"},
	}
	for _, c := range cases {
		got := m.findDonor(dir, "E", c.w, c.h, c.cover)
		if c.want != "" {
			c.want = filepath.Join(dir, c.want)
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, filepath.Base(got), filepath.Base(c.want))
		}
	}
	// only crops exist, asking for a fit: nothing usable
	dir2 := t.TempDir()
	fakeJPEG(t, filepath.Join(dir2, "1024x1024-cover-E.jpg"), 1024, 1024)
	if got := m.findDonor(dir2, "E", 256, 256, false); got != "" {
		t.Errorf("fit must not be derived from a crop, got %q", got)
	}
}

func TestExcludedExtensions(t *testing.T) {
	cfg := config.Default().Preview
	cfg.ExcludeExt = []string{"PDF", ".mov", " avi ", ""}
	m := New(cfg, t.TempDir())
	for name, want := range map[string]bool{
		"a.jpg": true, "b.HEIC": true, "c.mp4": true, "d.png": true, // still supported
		"e.pdf": false, "E.PDF": false, "f.mov": false, "g.MOV": false, "h.avi": false, // excluded, any case
		"i.txt": false, // never supported anyway
	} {
		if got := m.Supported(name); got != want {
			t.Errorf("Supported(%q) = %v, want %v", name, got, want)
		}
	}
	// excluded files must not even be queued or rendered
	if _, err := m.Get(1, "e", "/nonexistent", "x.pdf", 256, 256, false); err != ErrUnsupported {
		t.Errorf("Get on an excluded extension: %v", err)
	}
	m.Enqueue(1, "e", "/nonexistent", "x.pdf") // must be a no-op
	if len(m.queue) != 0 {
		t.Error("an excluded file was queued for pre-generation")
	}
}
