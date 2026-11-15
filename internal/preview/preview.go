// Package preview renders JPEG thumbnails for images, HEIC, video and PDF using
// external tools (ffmpeg, heif-convert, pdftoppm) and keeps them in an on-disk cache.
package preview

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nclite/internal/config"
)

var ErrUnsupported = errors.New("preview not supported")

type kind int

const (
	kNone kind = iota
	kImage
	kHEIC
	kVideo
	kPDF
)

func kindOf(name string) kind {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".tif", ".tiff":
		return kImage
	case ".heic", ".heif":
		return kHEIC
	case ".mp4", ".mov", ".m4v", ".mkv", ".webm", ".avi", ".3gp", ".mts":
		return kVideo
	case ".pdf":
		return kPDF
	}
	return kNone
}

func isJPEG(name string) bool {
	e := strings.ToLower(path.Ext(name))
	return e == ".jpg" || e == ".jpeg"
}

type job struct {
	id        int64
	etag, src string
	name      string
}

type call struct {
	done chan struct{}
	err  error
}

type Manager struct {
	cfg      config.Preview
	cacheDir string
	nicePath string
	sem      chan struct{}
	queue    chan job
	skip     map[string]bool // extensions excluded in the config, e.g. ".pdf"

	mu       sync.Mutex
	inflight map[string]*call

	heifLegacy atomic.Bool  // converter only understands "in out.jpg"
	derived    atomic.Int64 // previews made from a larger cached preview instead of the original
}

// Derived is how many previews so far were scaled down from an existing larger
// preview rather than decoded from the original file.
func (m *Manager) Derived() int64 { return m.derived.Load() }

func New(cfg config.Preview, cacheDir string) *Manager {
	// biggest size first: the smaller ones are then made from it, not from the original
	cfg.Pregen = append([]int(nil), cfg.Pregen...)
	sort.Sort(sort.Reverse(sort.IntSlice(cfg.Pregen)))
	m := &Manager{
		cfg:      cfg,
		cacheDir: cacheDir,
		sem:      make(chan struct{}, cfg.Workers),
		queue:    make(chan job, 2048),
		inflight: map[string]*call{},
		skip:     excludedExt(cfg.ExcludeExt),
	}
	if cfg.Nice > 0 {
		m.nicePath, _ = exec.LookPath("nice")
	}
	if cfg.Enabled {
		os.MkdirAll(filepath.Join(cacheDir, "tmp"), 0o755)
		go m.pregenLoop()
	}
	return m
}

// excludedExt normalises the configured list: lower case, with a leading dot.
func excludedExt(list []string) map[string]bool {
	out := map[string]bool{}
	for _, e := range list {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out[e] = true
	}
	return out
}

// Supported reports whether a preview can be made, and is wanted, for this file name.
// It is the single gate: the file listing (has-preview), the preview endpoints, the
// background pre-generation and the admin job all ask it, so an excluded extension
// is never rendered and never advertised to the app.
func (m *Manager) Supported(name string) bool {
	return m.cfg.Enabled && kindOf(name) != kNone && !m.skip[strings.ToLower(path.Ext(name))]
}

// Bucket maps a requested pixel size to the size actually rendered and cached.
func Bucket(n int) int { return bucket(n) }

func bucket(n int) int {
	for _, b := range []int{64, 128, 256, 512, 1024, 2048} {
		if n <= b {
			return b
		}
	}
	return 2048
}

// Enqueue schedules background rendering of the configured pregen sizes.
// It never blocks; when the queue is full the job is dropped (the preview is
// then rendered lazily on first request).
func (m *Manager) Enqueue(id int64, etag, src, name string) {
	if !m.Supported(name) || len(m.cfg.Pregen) == 0 {
		return
	}
	select {
	case m.queue <- job{id, etag, src, name}:
	default:
	}
}

func (m *Manager) pregenLoop() {
	for j := range m.queue {
		for _, sz := range m.cfg.Pregen {
			if _, err := m.Get(j.id, j.etag, j.src, j.name, sz, sz, false); err != nil {
				slog.Debug("pregen failed", "file", j.name, "err", err)
			}
		}
	}
}

// Get returns the path of a cached preview, rendering it first if needed.
// Concurrent requests for the same preview share one render.
func (m *Manager) Get(id int64, etag, src, name string, w, h int, cover bool) (string, error) {
	if !m.Supported(name) {
		return "", ErrUnsupported
	}
	w, h = normSize(w, h)
	dir, dst := m.cachePath(id, etag, w, h, cover)
	if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
		return dst, nil
	}

	m.mu.Lock()
	if c, ok := m.inflight[dst]; ok {
		m.mu.Unlock()
		<-c.done
		return dst, c.err
	}
	c := &call{done: make(chan struct{})}
	m.inflight[dst] = c
	m.mu.Unlock()

	c.err = m.render(src, name, dir, dst, etag, w, h, cover)

	m.mu.Lock()
	delete(m.inflight, dst)
	m.mu.Unlock()
	close(c.done)
	return dst, c.err
}

func normSize(w, h int) (int, int) {
	if w <= 0 {
		w = 256
	}
	if h <= 0 {
		h = 256
	}
	return bucket(w), bucket(h)
}

func (m *Manager) cachePath(id int64, etag string, w, h int, cover bool) (dir, dst string) {
	mode := "fit"
	if cover {
		mode = "cover"
	}
	dir = filepath.Join(m.cacheDir, strconv.FormatInt(id, 10))
	return dir, filepath.Join(dir, fmt.Sprintf("%dx%d-%s-%s.jpg", w, h, mode, etag))
}

// Has reports whether this exact preview is already cached (nothing is rendered).
func (m *Manager) Has(id int64, etag string, w, h int, cover bool) bool {
	w, h = normSize(w, h)
	_, dst := m.cachePath(id, etag, w, h, cover)
	st, err := os.Stat(dst)
	return err == nil && st.Size() > 0
}

func (m *Manager) render(src, name, dir, dst, etag string, w, h int, cover bool) error {
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(m.cfg.TimeoutSec)*time.Second)
	defer cancel()

	part := dst + ".part.jpg"
	defer os.Remove(part)

	var err error
	if donor := m.findDonor(dir, etag, w, h, cover); donor != "" {
		if err = m.renderDerived(ctx, donor, part, w, h, cover); err == nil {
			m.derived.Add(1)
			return m.finish(part, dst, dir, etag)
		}
		os.Remove(part) // donor unusable: fall back to the original below
		slog.Debug("derive failed, using original", "donor", donor, "err", err)
	}
	switch kindOf(name) {
	case kImage:
		err = m.renderImage(ctx, src, part, name, w, h, cover)
	case kHEIC:
		err = m.renderViaIntermediate(ctx, src, part, w, h, cover, func(tmp string) (string, error) {
			return m.heifToJPEG(ctx, src, tmp)
		})
	case kPDF:
		err = m.renderViaIntermediate(ctx, src, part, w, h, cover, func(tmp string) (string, error) {
			return m.pdfToJPEG(ctx, src, tmp, max(w, h))
		})
	case kVideo:
		err = m.renderVideo(ctx, src, part, w, h, cover)
	default:
		err = ErrUnsupported
	}
	if err != nil {
		return err
	}
	return m.finish(part, dst, dir, etag)
}

// finish publishes a rendered temp file and drops previews of older file versions.
func (m *Manager) finish(part, dst, dir, etag string) error {
	if st, e := os.Stat(part); e != nil || st.Size() == 0 {
		return errors.New("renderer produced no output")
	}
	if err := os.Rename(part, dst); err != nil {
		return err
	}
	keep := "-" + etag + ".jpg"
	if ents, e := os.ReadDir(dir); e == nil {
		for _, en := range ents {
			if !strings.HasSuffix(en.Name(), keep) && !strings.Contains(en.Name(), ".part.") {
				os.Remove(filepath.Join(dir, en.Name()))
			}
		}
	}
	return nil
}

// findDonor looks for an already rendered preview of this exact file version that
// is big enough to make (w, h, cover) from, and returns the smallest such one: it
// is the cheapest to decode and loses the least when scaled down. Rules:
//   - a "fit" preview can be made from any larger "fit" preview (its box is bigger
//     in both directions, so it holds at least the pixels needed);
//   - a cropped ("cover") preview can never give back its cropped-away edges, so
//     it only serves cover previews of the same aspect ratio;
//   - a "cover" preview can be made from a larger "fit" one if that one still has
//     at least w x h real pixels (checked in the JPEG header).
//
// Returns "" when nothing qualifies and the original has to be decoded.
func (m *Manager) findDonor(dir, etag string, w, h int, cover bool) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	suffix := "-" + etag + ".jpg"
	best, bestArea := "", 0
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, suffix) || strings.Contains(name, ".part") {
			continue
		}
		var cw, ch int
		var mode string
		if n, _ := fmt.Sscanf(strings.TrimSuffix(name, suffix), "%dx%d-%s", &cw, &ch, &mode); n != 3 {
			continue
		}
		donorCover := mode == "cover"
		if cw < w || ch < h || (cw == w && ch == h && donorCover == cover) {
			continue
		}
		file := filepath.Join(dir, name)
		switch {
		case !cover && donorCover:
			continue
		case cover && donorCover && cw*h != ch*w:
			continue
		case cover && !donorCover:
			if _, pw, ph := jpegInfo(file); pw < w || ph < h {
				continue
			}
		}
		if area := cw * ch; best == "" || area < bestArea {
			best, bestArea = file, area
		}
	}
	return best
}

func (m *Manager) renderDerived(ctx context.Context, donor, out string, w, h int, cover bool) error {
	_, sw, sh := jpegInfo(donor) // our own previews carry no EXIF; they are already upright
	return m.ffmpegShrink(ctx, donor, out, w, h, cover, 1, sw, sh)
}

func (m *Manager) renderImage(ctx context.Context, src, out, name string, w, h int, cover bool) error {
	orient, sw, sh := 1, 0, 0
	if isJPEG(name) {
		orient, sw, sh = jpegInfo(src)
	}
	return m.ffmpegShrink(ctx, src, out, w, h, cover, orient, sw, sh)
}

// ffmpegShrink scales in to out. When the source is a JPEG of known size it asks
// the decoder for a 1/2, 1/4 or 1/8 size decode (shrink-on-load), which is far
// cheaper than decoding the full frame and then scaling down.
func (m *Manager) ffmpegShrink(ctx context.Context, in, out string, w, h int, cover bool, orient, sw, sh int) error {
	vf := filter(w, h, cover, orient)
	lowres := 0
	if sw > 0 && sh > 0 {
		// f = how much the image is scaled: "fit" shrinks to the box (smaller factor),
		// "cover" to the box's far side (larger factor). A 1/2^k decode is only safe
		// while it is still at least as big as the result.
		fw, fh := float64(w)/float64(sw), float64(h)/float64(sh)
		f := math.Min(fw, fh)
		if cover {
			f = math.Max(fw, fh)
		}
		for lowres < 3 && f <= 1/float64(int(2)<<lowres) {
			lowres++
		}
	}
	if lowres > 0 {
		if err := m.ffmpeg(ctx, out, vf, []string{"-noautorotate", "-lowres", strconv.Itoa(lowres)}, in); err == nil {
			return nil
		}
		os.Remove(out)
	}
	return m.ffmpeg(ctx, out, vf, []string{"-noautorotate"}, in)
}

// renderViaIntermediate converts the source to a temporary JPEG first (HEIC, PDF).
func (m *Manager) renderViaIntermediate(ctx context.Context, src, out string, w, h int, cover bool,
	conv func(tmp string) (string, error)) error {
	// created here, not only at startup: someone may clean the cache directory while we run
	if err := os.MkdirAll(filepath.Join(m.cacheDir, "tmp"), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(m.cacheDir, "tmp", "i-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	got, err := conv(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(got)
	// libheif and poppler already apply orientation, so no rotation here
	_, sw, sh := jpegInfo(got)
	return m.ffmpegShrink(ctx, got, out, w, h, cover, 1, sw, sh)
}

// heifToJPEG decodes a HEIC/HEIF file to a temporary full-size JPEG and returns its path.
//
// libheif's converter has two command line dialects:
//
//	new (heif-dec, libheif >= 1.19): heif-convert -q 85 in.heic -o <base> -f jpg   (".jpg" is appended)
//	old:                             heif-convert -q 85 in.heic <base>.jpg
//
// The dialect that worked last time is tried first. Success is judged by the
// output file, not the exit status: old versions answer an unknown option with
// their usage text and exit code 0.
func (m *Manager) heifToJPEG(ctx context.Context, src, tmp string) (string, error) {
	newArgs := []string{"-q", "85", src, "-o", tmp, "-f", "jpg"}
	oldArgs := []string{"-q", "85", src, tmp + ".jpg"}
	first, second, firstIsNew := newArgs, oldArgs, true
	if m.heifLegacy.Load() {
		first, second, firstIsNew = oldArgs, newArgs, false
	}
	err := m.run(ctx, m.cfg.HeifConvert, first...)
	if out, ok := findHeifOutput(tmp); ok {
		return out, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	err2 := m.run(ctx, m.cfg.HeifConvert, second...)
	if out, ok := findHeifOutput(tmp); ok {
		m.heifLegacy.Store(firstIsNew) // remember which dialect this build speaks
		return out, nil
	}
	if err == nil {
		err = err2
	}
	if err == nil {
		err = errors.New("heif-convert produced no output")
	}
	return "", err
}

// findHeifOutput locates the JPEG written for <tmp>: "<tmp>.jpg", or for files
// holding several images "<tmp>-1.jpg", "<tmp>-2.jpg", ... (the first one is used).
func findHeifOutput(tmp string) (string, bool) {
	if st, err := os.Stat(tmp + ".jpg"); err == nil && st.Size() > 0 {
		return tmp + ".jpg", true
	}
	matches, _ := filepath.Glob(tmp + "-*.jpg")
	if len(matches) == 0 {
		return "", false
	}
	sort.Strings(matches)
	for _, extra := range matches[1:] {
		os.Remove(extra)
	}
	return matches[0], true
}

func (m *Manager) pdfToJPEG(ctx context.Context, src, tmp string, size int) (string, error) {
	if err := m.run(ctx, m.cfg.Pdftoppm, "-jpeg", "-singlefile", "-f", "1", "-l", "1",
		"-scale-to", strconv.Itoa(size), src, tmp); err != nil {
		return "", err
	}
	return tmp + ".jpg", nil
}

func (m *Manager) renderVideo(ctx context.Context, src, out string, w, h int, cover bool) error {
	vf := filter(w, h, cover, 1)
	// one second in avoids the black first frame; very short clips need seek 0
	for _, ss := range []string{"1", "0"} {
		os.Remove(out)
		err := m.ffmpeg(ctx, out, vf, []string{"-ss", ss}, src)
		if st, e := os.Stat(out); err == nil && e == nil && st.Size() > 0 {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return errors.New("no video frame extracted")
}

func (m *Manager) ffmpeg(ctx context.Context, out, vf string, inOpts []string, in string) error {
	args := []string{"-nostdin", "-v", "error", "-y"}
	args = append(args, inOpts...)
	args = append(args, "-i", in, "-frames:v", "1", "-vf", vf,
		"-c:v", "mjpeg", "-q:v", strconv.Itoa(m.cfg.Quality), "-f", "image2", "-update", "1", out)
	return m.run(ctx, m.cfg.FFmpeg, args...)
}

func filter(w, h int, cover bool, orient int) string {
	var f []string
	switch orient {
	case 2:
		f = append(f, "hflip")
	case 3:
		f = append(f, "hflip", "vflip")
	case 4:
		f = append(f, "vflip")
	case 5:
		f = append(f, "transpose=0")
	case 6:
		f = append(f, "transpose=1")
	case 7:
		f = append(f, "transpose=3")
	case 8:
		f = append(f, "transpose=2")
	}
	if cover {
		f = append(f, fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase", w, h), fmt.Sprintf("crop=%d:%d", w, h))
	} else {
		f = append(f, fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease", w, h))
	}
	return strings.Join(f, ",")
}

type capBuf struct{ b bytes.Buffer }

func (c *capBuf) Write(p []byte) (int, error) {
	if c.b.Len() < 1024 {
		c.b.Write(p)
	}
	return len(p), nil
}

func (m *Manager) run(ctx context.Context, bin string, args ...string) error {
	name := bin
	if m.nicePath != "" {
		args = append([]string{"-n", strconv.Itoa(m.cfg.Nice), bin}, args...)
		name = m.nicePath
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var eb capBuf
	cmd.Stderr = &eb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", filepath.Base(bin), err, strings.TrimSpace(eb.b.String()))
	}
	return nil
}

// GC removes cached previews of files that no longer exist in the index.
func (m *Manager) GC(exists func(id int64) bool) {
	ents, err := os.ReadDir(m.cacheDir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if id, err := strconv.ParseInt(e.Name(), 10, 64); err == nil && e.IsDir() && !exists(id) {
			os.RemoveAll(filepath.Join(m.cacheDir, e.Name()))
		}
	}
	// stale intermediates from crashed renders
	if tmp, err := os.ReadDir(filepath.Join(m.cacheDir, "tmp")); err == nil {
		for _, e := range tmp {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
				os.Remove(filepath.Join(m.cacheDir, "tmp", e.Name()))
			}
		}
	}
}

// ---- JPEG header parsing (EXIF orientation + dimensions) -------------------

// jpegInfo returns the EXIF orientation (1..8) and the pixel size of a JPEG.
// Anything unparsable yields orientation 1 and zero size.
func jpegInfo(file string) (orient, w, h int) {
	orient = 1
	f, err := os.Open(file)
	if err != nil {
		return
	}
	defer f.Close()
	buf := make([]byte, 256*1024)
	n, _ := io.ReadFull(f, buf)
	buf = buf[:n]
	if len(buf) < 4 || buf[0] != 0xFF || buf[1] != 0xD8 {
		return
	}
	i := 2
	for i+4 <= len(buf) {
		if buf[i] != 0xFF {
			return
		}
		mk := buf[i+1]
		if mk == 0xFF {
			i++
			continue
		}
		if mk == 0xDA || mk == 0xD9 {
			return
		}
		l := int(buf[i+2])<<8 | int(buf[i+3])
		switch {
		case mk == 0xE1 && i+10 <= len(buf) && string(buf[i+4:i+10]) == "Exif\x00\x00":
			end := min(i+2+l, len(buf))
			orient = tiffOrientation(buf[i+10 : end])
		case (mk == 0xC0 || mk == 0xC1 || mk == 0xC2) && i+9 <= len(buf):
			h = int(buf[i+5])<<8 | int(buf[i+6])
			w = int(buf[i+7])<<8 | int(buf[i+8])
			return
		}
		i += 2 + l
	}
	return
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 1
	}
	cnt := int(bo.Uint16(t[off : off+2]))
	for k := 0; k < cnt; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:e+2]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8 : e+10])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}
