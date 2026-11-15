package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"nclite/internal/preview"
	"nclite/internal/store"
)

// A job is a long-running maintenance task started from the admin page. Only one
// runs at a time and each works strictly one file at a time (previews additionally
// go through the preview worker limit and `nice`), so a job never takes more than
// a single core and a single decode's worth of memory.

type sizeSpec struct {
	W, H  int
	Cover bool
}

func (z sizeSpec) String() string {
	s := fmt.Sprintf("%dx%d", z.W, z.H)
	if z.Cover {
		s += "c"
	}
	return s
}

var reSize = regexp.MustCompile(`^(\d{2,4})(?:x(\d{2,4}))?(c)?$`)

// parseSizes reads "512, 1024x768, 256c" (a bare number is a square, a trailing
// "c" means crop-to-fill instead of fit) and snaps each side to the cached buckets.
func parseSizes(s string) ([]sizeSpec, error) {
	var out []sizeSpec
	seen := map[sizeSpec]bool{}
	for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		m := reSize.FindStringSubmatch(tok)
		if m == nil {
			return nil, fmt.Errorf("bad size %q (examples: 512, 1024x768, 256c)", tok)
		}
		w, _ := strconv.Atoi(m[1])
		h := w
		if m[2] != "" {
			h, _ = strconv.Atoi(m[2])
		}
		z := sizeSpec{preview.Bucket(w), preview.Bucket(h), m[3] == "c"}
		if !seen[z] {
			seen[z] = true
			out = append(out, z)
		}
	}
	if len(out) == 0 || len(out) > 6 {
		return nil, errors.New("give between 1 and 6 sizes")
	}
	// Biggest first (and "fit" before "cover" at equal size): the first one is decoded
	// from the original, every smaller one is then scaled down from it, which is far
	// cheaper than decoding a HEIC or a video again for each size.
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := out[i].W*out[i].H, out[j].W*out[j].H
		if ai != aj {
			return ai > aj
		}
		return !out[i].Cover && out[j].Cover
	})
	return out, nil
}

type Job struct {
	mu      sync.Mutex
	Kind    string // "scan" | "previews"
	Users   []string
	AllUsrs bool
	Path    string
	Sizes   []sizeSpec

	State             string // running | done | cancelled | failed
	Phase             string
	Started, Finished time.Time
	busy              time.Duration // time spent rendering, for the ETA
	derivedFn         func() int64  // preview manager's "made from a larger preview" counter
	derivedBase       int64

	Dirs, Files, Bytes                    int64
	Total, Done, Rendered, Cached, Failed int64
	Current, Err                          string
	cancel                                context.CancelFunc
}

// JobView is an immutable snapshot for the template.
type JobView struct {
	Title, State, Phase, Current, Err, Elapsed, ETA, Bytes string
	Running, Determinate                                   bool
	Total, Done, Rendered, Cached, Failed, Dirs, Files     int64
	IsPreviews                                             bool
	Derived                                                int64
}

func (j *Job) running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.State == "running"
}

func (j *Job) view() JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	target := "all users"
	if !j.AllUsrs {
		target = strings.Join(j.Users, ", ")
		if j.Path != "" {
			target += "/" + j.Path
		}
	}
	title := "Index scan: " + target
	if j.Kind == "previews" {
		var sz []string
		for _, z := range j.Sizes {
			sz = append(sz, z.String())
		}
		title = "Previews (" + strings.Join(sz, ", ") + "): " + target
	}
	end := j.Finished
	if end.IsZero() {
		end = time.Now()
	}
	v := JobView{
		Title: title, State: j.State, Phase: j.Phase, Current: j.Current, Err: j.Err,
		Elapsed: end.Sub(j.Started).Round(time.Second).String(), Running: j.State == "running",
		Total: j.Total, Done: j.Done, Rendered: j.Rendered, Cached: j.Cached, Failed: j.Failed,
		Dirs: j.Dirs, Files: j.Files, Bytes: human(j.Bytes), IsPreviews: j.Kind == "previews",
		Determinate: j.Kind == "previews" && j.Phase == "rendering" && j.Total > 0,
	}
	if j.derivedFn != nil {
		v.Derived = j.derivedFn() - j.derivedBase
	}
	if v.Running && j.Kind == "previews" && j.Rendered > 0 && j.Done < j.Total {
		// upper bound: assumes every remaining file still needs rendering
		rem := time.Duration(j.Total-j.Done) * (j.busy / time.Duration(j.Rendered))
		v.ETA = "at most ~" + rem.Round(time.Second).String()
	}
	return v
}

func (s *Server) currentJob() *Job {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	return s.job
}

func (s *Server) startJob(j *Job) error {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	if s.job != nil && s.job.running() {
		return errors.New("another job is still running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.cancel, j.State, j.Started = cancel, "running", time.Now()
	s.job = j
	go s.runJob(ctx, j)
	return nil
}

func (s *Server) runJob(ctx context.Context, j *Job) {
	defer func() {
		if r := recover(); r != nil {
			j.mu.Lock()
			j.Err = fmt.Sprint("internal error: ", r)
			j.mu.Unlock()
			slog.Error("job panicked", "err", r)
		}
		j.mu.Lock()
		j.Finished, j.Current, j.Phase = time.Now(), "", ""
		switch {
		case ctx.Err() != nil:
			j.State = "cancelled"
		case j.Err != "":
			j.State = "failed"
		default:
			j.State = "done"
		}
		j.mu.Unlock()
		j.cancel()
	}()
	switch j.Kind {
	case "scan":
		s.runScan(ctx, j)
	case "previews":
		s.runPreviews(ctx, j)
	}
}

func (j *Job) fail(user string, err error) {
	j.mu.Lock()
	j.Err = user + ": " + err.Error()
	j.mu.Unlock()
}

func (s *Server) runScan(ctx context.Context, j *Job) {
	for _, u := range j.Users {
		j.mu.Lock()
		j.Phase = "indexing " + u
		j.mu.Unlock()
		err := s.fs.Walk(ctx, u, j.Path, func(dir string, nodes []*store.Node) bool {
			j.mu.Lock()
			j.Dirs++
			j.Current = u + "/" + dir
			for _, n := range nodes {
				if !n.IsDir {
					j.Files++
					j.Bytes += n.Size
				}
			}
			j.mu.Unlock()
			return true
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			j.fail(u, err)
			return
		}
		if j.Path == "" {
			s.st.SetMeta("lastscan:"+u, strconv.FormatInt(time.Now().Unix(), 10))
		}
	}
}

func (s *Server) runPreviews(ctx context.Context, j *Job) {
	j.mu.Lock()
	j.derivedFn, j.derivedBase = s.pv.Derived, s.pv.Derived()
	j.mu.Unlock()
	// pass 1: index and count the files that can have a preview, so there is a total to show
	j.mu.Lock()
	j.Phase = "counting"
	j.mu.Unlock()
	for _, u := range j.Users {
		err := s.fs.Walk(ctx, u, j.Path, func(dir string, nodes []*store.Node) bool {
			j.mu.Lock()
			j.Dirs++
			j.Current = u + "/" + dir
			for _, n := range nodes {
				if !n.IsDir {
					j.Files++
					if s.pv.Supported(path.Base(n.Path)) {
						j.Total++
					}
				}
			}
			j.mu.Unlock()
			return true
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			j.fail(u, err)
			return
		}
	}

	// pass 2: render, one file at a time
	j.mu.Lock()
	j.Phase = "rendering"
	j.mu.Unlock()
	for _, u := range j.Users {
		s.fs.Walk(ctx, u, j.Path, func(dir string, nodes []*store.Node) bool {
			for _, n := range nodes {
				if ctx.Err() != nil {
					return false
				}
				name := path.Base(n.Path)
				if n.IsDir || !s.pv.Supported(name) {
					continue
				}
				s.renderOne(j, u, n, name)
			}
			return true
		})
		if ctx.Err() != nil {
			return
		}
	}
}

func (s *Server) renderOne(j *Job, owner string, n *store.Node, name string) {
	src := s.fs.Real(owner, n.Path)
	allCached, failed := true, false
	t0 := time.Now()
	for _, z := range j.Sizes {
		if s.pv.Has(n.ID, n.ETag, z.W, z.H, z.Cover) {
			continue
		}
		allCached = false
		if _, err := s.pv.Get(n.ID, n.ETag, src, name, z.W, z.H, z.Cover); err != nil {
			failed = true
			slog.Warn("preview job: render failed", "file", owner+"/"+n.Path, "err", err)
		}
	}
	j.mu.Lock()
	j.Done++
	j.Current = owner + "/" + n.Path
	switch {
	case failed:
		j.Failed++
	case allCached:
		j.Cached++
	default:
		j.Rendered++
		j.busy += time.Since(t0)
	}
	j.mu.Unlock()
}

// ---- which preview sizes do the apps really ask for? ----------------------------

type sizeStat struct {
	Spec sizeSpec
	N    int64
}

func (s *Server) noteSize(x, y int, cover bool) {
	if x <= 0 {
		x = 256
	}
	if y <= 0 {
		y = 256
	}
	z := sizeSpec{preview.Bucket(x), preview.Bucket(y), cover}
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	if s.sizeReq == nil {
		s.sizeReq = map[sizeSpec]int64{}
	}
	if _, ok := s.sizeReq[z]; ok || len(s.sizeReq) < 64 {
		s.sizeReq[z]++
	}
}

// topSizes returns the most requested preview sizes since the server started.
func (s *Server) topSizes(n int) []sizeStat {
	s.sizeMu.Lock()
	out := make([]sizeStat, 0, len(s.sizeReq))
	for z, c := range s.sizeReq {
		out = append(out, sizeStat{z, c})
	}
	s.sizeMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].N > out[j].N })
	if len(out) > n {
		out = out[:n]
	}
	return out
}
