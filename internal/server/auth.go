package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"nclite/internal/store"
)

// Password hashing: PBKDF2-HMAC-SHA256, stdlib only. It is used for browser /
// login-flow logins only. Devices authenticate with random app passwords whose
// SHA-256 is looked up directly, so the hot path never runs a slow KDF.
const pbkdf2Iter = 120000

func pbkdf2(pw, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, pw)
	hLen := prf.Size()
	blocks := (keyLen + hLen - 1) / hLen
	dk := make([]byte, 0, blocks*hLen)
	u := make([]byte, hLen)
	for b := 1; b <= blocks; b++ {
		prf.Reset()
		prf.Write(salt)
		prf.Write([]byte{byte(b >> 24), byte(b >> 16), byte(b >> 8), byte(b)})
		dk = prf.Sum(dk)
		t := dk[len(dk)-hLen:]
		copy(u, t)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for x := range u {
				t[x] ^= u[x]
			}
		}
	}
	return dk[:keyLen]
}

func HashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key := pbkdf2([]byte(pw), salt, pbkdf2Iter, 32)
	return fmt.Sprintf("pbkdf2$%d$%s$%s", pbkdf2Iter, base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

func CheckPassword(stored, pw string) bool {
	p := strings.Split(stored, "$")
	if len(p) != 4 || p[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	if err != nil || iter < 1000 || iter > 5000000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(p[2])
	want, err2 := base64.RawStdEncoding.DecodeString(p[3])
	if err1 != nil || err2 != nil {
		return false
	}
	return subtle.ConstantTimeCompare(pbkdf2([]byte(pw), salt, iter, len(want)), want) == 1
}

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// newAppPassword returns a random 43-character password and its SHA-256.
func newAppPassword() (plain, hash string) {
	b := make([]byte, 32)
	rand.Read(b)
	plain = base64.RawURLEncoding.EncodeToString(b)
	return plain, sha256hex(plain)
}

// ---- request authentication --------------------------------------------------

type failEntry struct {
	n            int
	first, until time.Time
}

type Authenticator struct {
	st *store.Store

	mu    sync.Mutex
	okPw  map[string]time.Time // sha256(user\0pw) -> expiry, saves repeating PBKDF2
	fails map[string]*failEntry
}

func NewAuthenticator(st *store.Store) *Authenticator {
	return &Authenticator{st: st, okPw: map[string]time.Time{}, fails: map[string]*failEntry{}}
}

func (a *Authenticator) blocked(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.fails[ip]
	return e != nil && time.Now().Before(e.until)
}

func (a *Authenticator) fail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if len(a.fails) > 10000 {
		a.fails = map[string]*failEntry{}
	}
	e := a.fails[ip]
	if e == nil || now.Sub(e.first) > 10*time.Minute {
		e = &failEntry{first: now}
		a.fails[ip] = e
	}
	if e.n++; e.n >= 8 {
		e.until, e.n = now.Add(10*time.Minute), 0
	}
}

// InvalidatePasswords forgets cached password verifications (after a password change).
func (a *Authenticator) InvalidatePasswords() {
	a.mu.Lock()
	a.okPw = map[string]time.Time{}
	a.mu.Unlock()
}

var errBlocked = fmt.Errorf("too many failed attempts")

// CheckPassword verifies a real account password (browser / login flow).
func (a *Authenticator) CheckPassword(user, pw, ip string) (bool, error) {
	if a.blocked(ip) {
		return false, errBlocked
	}
	key := sha256hex(user + "\x00" + pw)
	a.mu.Lock()
	exp, hit := a.okPw[key]
	a.mu.Unlock()
	if hit && time.Now().Before(exp) {
		return true, nil
	}
	u, _ := a.st.GetUser(user)
	if u == nil || !CheckPassword(u.PassHash, pw) {
		a.fail(ip)
		return false, nil
	}
	a.mu.Lock()
	if len(a.okPw) > 1000 {
		a.okPw = map[string]time.Time{}
	}
	a.okPw[key] = time.Now().Add(10 * time.Minute)
	a.mu.Unlock()
	return true, nil
}

// Basic authenticates an API request: app password first, account password as fallback.
func (a *Authenticator) Basic(r *http.Request, ip string) (user string, ok bool, err error) {
	name, pw, has := r.BasicAuth()
	if !has || name == "" || pw == "" {
		return "", false, nil
	}
	if a.blocked(ip) {
		return "", false, errBlocked
	}
	if u := a.st.UserByAppHash(sha256hex(pw)); u != "" && u == name {
		return name, true, nil
	}
	ok, err = a.CheckPassword(name, pw, ip)
	if ok {
		return name, true, nil
	}
	return "", false, err
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if v := r.Header.Get("X-Real-IP"); v != "" {
			return v
		}
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			ip, _, _ := strings.Cut(v, ",")
			return strings.TrimSpace(ip)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// authed authenticates the request or writes the 401/429 itself.
func (s *Server) authed(w http.ResponseWriter, r *http.Request) (string, bool) {
	user, ok, err := s.au.Basic(r, s.clientIP(r))
	if ok {
		return user, true
	}
	if err == errBlocked {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return "", false
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="`+s.cfg.ProductName+`", charset="UTF-8"`)
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
	return "", false
}
