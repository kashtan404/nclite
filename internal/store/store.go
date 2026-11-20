// Package store keeps users, app passwords, shares and the file index in SQLite.
//
// Files live on disk as-is; the index only assigns stable file ids and etags.
// Folder etags change whenever anything below them changes (propagated to the
// root) because that is how the Nextcloud client decides what to sync.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"strings"
	"time"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS users(
  name TEXT PRIMARY KEY, display TEXT NOT NULL, pass_hash TEXT NOT NULL,
  is_admin INTEGER NOT NULL DEFAULT 0, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS app_passwords(
  id INTEGER PRIMARY KEY, user TEXT NOT NULL REFERENCES users(name) ON DELETE CASCADE,
  hash TEXT NOT NULL UNIQUE, label TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS app_passwords_user ON app_passwords(user);
CREATE TABLE IF NOT EXISTS nodes(
  id INTEGER PRIMARY KEY AUTOINCREMENT, owner TEXT NOT NULL, path TEXT NOT NULL, dir TEXT NOT NULL,
  is_dir INTEGER NOT NULL, size INTEGER NOT NULL, mtime INTEGER NOT NULL, etag TEXT NOT NULL,
  UNIQUE(owner, path));
CREATE INDEX IF NOT EXISTS nodes_dir ON nodes(owner, dir);
CREATE INDEX IF NOT EXISTS nodes_dir_nocase ON nodes(owner, dir, path COLLATE NOCASE);
CREATE TABLE IF NOT EXISTS favorites(
  user TEXT NOT NULL, node_id INTEGER NOT NULL, PRIMARY KEY(user, node_id)) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS favorites_node ON favorites(node_id);
CREATE TABLE IF NOT EXISTS trash(
  id INTEGER PRIMARY KEY AUTOINCREMENT, owner TEXT NOT NULL, name TEXT NOT NULL, orig_dir TEXT NOT NULL,
  deleted_at INTEGER NOT NULL, is_dir INTEGER NOT NULL, size INTEGER NOT NULL, UNIQUE(owner, name, deleted_at));
CREATE TABLE IF NOT EXISTS direct(
  token TEXT PRIMARY KEY, user TEXT NOT NULL, node_id INTEGER NOT NULL, expires INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS albums(
  id INTEGER PRIMARY KEY AUTOINCREMENT, owner TEXT NOT NULL, name TEXT NOT NULL, created INTEGER NOT NULL,
  UNIQUE(owner, name));
CREATE TABLE IF NOT EXISTS album_items(
  album_id INTEGER NOT NULL, node_id INTEGER NOT NULL, added INTEGER NOT NULL, seq INTEGER NOT NULL,
  PRIMARY KEY(album_id, node_id)) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS album_items_node ON album_items(node_id);
CREATE TABLE IF NOT EXISTS shares(
  id INTEGER PRIMARY KEY, owner TEXT NOT NULL, path TEXT NOT NULL, target TEXT NOT NULL,
  readonly INTEGER NOT NULL DEFAULT 0, created INTEGER NOT NULL, UNIQUE(owner, path, target));
`

type Store struct{ db *sql.DB }

// ErrStale: the data a caller prepared no longer matches the index; read it again.
var ErrStale = errors.New("stale directory snapshot")

func Open(path string) (*Store, error) {
	db, err := sql.Open(driverName, dsn(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// RandHex is exported for callers that need random tokens.
func RandHex(n int) string { return randHex(n) }

// ---- meta -----------------------------------------------------------------

// Meta returns a stored value, creating it with gen() on first use.
func (s *Store) Meta(key string, gen func() string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k=?`, key).Scan(&v)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	v = gen()
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta(k,v) VALUES(?,?)`, key, v); err != nil {
		return "", err
	}
	err = s.db.QueryRow(`SELECT v FROM meta WHERE k=?`, key).Scan(&v)
	return v, err
}

// ---- users ----------------------------------------------------------------

type User struct {
	Name, Display, PassHash string
	Admin                   bool
	Created                 int64
}

func (s *Store) CreateUser(name, passHash string, admin bool) error {
	_, err := s.db.Exec(`INSERT INTO users(name,display,pass_hash,is_admin,created) VALUES(?,?,?,?,?)`,
		name, name, passHash, b2i(admin), time.Now().Unix())
	return err
}

func (s *Store) GetUser(name string) (*User, error) {
	u := &User{}
	var adm int
	err := s.db.QueryRow(`SELECT name,display,pass_hash,is_admin,created FROM users WHERE name=?`, name).
		Scan(&u.Name, &u.Display, &u.PassHash, &adm, &u.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	u.Admin = adm == 1
	return u, err
}

func (s *Store) ListUsers() ([]*User, error) {
	rows, err := s.db.Query(`SELECT name,display,pass_hash,is_admin,created FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u := &User{}
		var adm int
		if err := rows.Scan(&u.Name, &u.Display, &u.PassHash, &adm, &u.Created); err != nil {
			return nil, err
		}
		u.Admin = adm == 1
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CountUsers() (n int) {
	s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return
}

func (s *Store) SetPassword(name, passHash string) error {
	r, err := s.db.Exec(`UPDATE users SET pass_hash=? WHERE name=?`, passHash, name)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteUser removes the account with everything the index knows about it
// (app passwords, shares, favorites, trash records, index rows). Files on disk stay.
func (s *Store) DeleteUser(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tr := TrashOwner(name)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM album_items WHERE album_id IN (SELECT id FROM albums WHERE owner=?) OR node_id IN (SELECT id FROM nodes WHERE owner=? OR owner=?)`, []any{name, name, tr}},
		{`DELETE FROM albums WHERE owner=?`, []any{name}},
		{`DELETE FROM favorites WHERE user=? OR node_id IN (SELECT id FROM nodes WHERE owner=? OR owner=?)`, []any{name, name, tr}},
		{`DELETE FROM shares WHERE owner=? OR target=?`, []any{name, name}},
		{`DELETE FROM nodes WHERE owner=? OR owner=?`, []any{name, tr}},
		{`DELETE FROM trash WHERE owner=?`, []any{name}},
		{`DELETE FROM direct WHERE user=?`, []any{name}},
		{`DELETE FROM app_passwords WHERE user=?`, []any{name}},
		{`DELETE FROM users WHERE name=?`, []any{name}},
	} {
		if _, err := tx.Exec(q.sql, q.args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- app passwords --------------------------------------------------------

func (s *Store) AddAppPassword(user, hash, label string) error {
	_, err := s.db.Exec(`INSERT INTO app_passwords(user,hash,label,created) VALUES(?,?,?,?)`,
		user, hash, label, time.Now().Unix())
	return err
}

// UserByAppHash returns "" when the hash is unknown.
func (s *Store) UserByAppHash(hash string) string {
	var u string
	s.db.QueryRow(`SELECT user FROM app_passwords WHERE hash=?`, hash).Scan(&u)
	return u
}

func (s *Store) DeleteAppPassword(hash string) error {
	_, err := s.db.Exec(`DELETE FROM app_passwords WHERE hash=?`, hash)
	return err
}

func (s *Store) DeleteAppPasswordsOf(user string) error {
	_, err := s.db.Exec(`DELETE FROM app_passwords WHERE user=?`, user)
	return err
}

func (s *Store) CountAppPasswords(user string) (n int) {
	s.db.QueryRow(`SELECT COUNT(*) FROM app_passwords WHERE user=?`, user).Scan(&n)
	return
}

// ---- shares ---------------------------------------------------------------

type Share struct {
	ID                  int64
	Owner, Path, Target string
	ReadOnly            bool
}

func (s *Store) ListShares() ([]Share, error) {
	rows, err := s.db.Query(`SELECT id,owner,path,target,readonly FROM shares ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Share
	for rows.Next() {
		var sh Share
		var ro int
		if err := rows.Scan(&sh.ID, &sh.Owner, &sh.Path, &sh.Target, &ro); err != nil {
			return nil, err
		}
		sh.ReadOnly = ro == 1
		out = append(out, sh)
	}
	return out, rows.Err()
}

func (s *Store) AddShare(owner, path, target string, ro bool) error {
	_, err := s.db.Exec(`INSERT INTO shares(owner,path,target,readonly,created) VALUES(?,?,?,?,?)`,
		owner, path, target, b2i(ro), time.Now().Unix())
	return err
}

func (s *Store) DeleteShare(id int64) error {
	_, err := s.db.Exec(`DELETE FROM shares WHERE id=?`, id)
	return err
}

// ---- file index -----------------------------------------------------------

type Node struct {
	ID          int64
	Owner       string
	Path, Dir   string // relative to the owner's files root, "" = root
	IsDir       bool
	Size, Mtime int64
	ETag        string
}

func ParentOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// Join appends name to dir. An empty name means "dir itself": joining it must not
// produce "dir/", a different path that would index the same folder a second time.
func Join(dir, name string) string {
	if dir == "" {
		return name
	}
	if name == "" {
		return dir
	}
	return dir + "/" + name
}

// badPath reports paths that are not in canonical form (leading or trailing slash,
// empty segment). Such a path is an alias of a real one and must never be indexed.
func badPath(p string) bool {
	return p != "" && (p[0] == '/' || p[len(p)-1] == '/' || strings.Contains(p, "//"))
}

var errBadPath = errors.New("refusing to index a non-canonical path")

// ancestors returns the strict ancestors of p, nearest first, ending with "".
func ancestors(p string) []string {
	var out []string
	for p != "" {
		p = ParentOf(p)
		out = append(out, p)
	}
	return out
}

type querier interface {
	QueryRow(string, ...any) *sql.Row
}

func scanNode(row *sql.Row, owner string) (*Node, error) {
	n := &Node{Owner: owner}
	var d int
	err := row.Scan(&n.ID, &n.Path, &n.Dir, &d, &n.Size, &n.Mtime, &n.ETag)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	n.IsDir = d == 1
	return n, err
}

const nodeCols = `id,path,dir,is_dir,size,mtime,etag`

func nodeAt(q querier, owner, p string) (*Node, error) {
	return scanNode(q.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE owner=? AND path=?`, owner, p), owner)
}

// NodeByPath returns (nil, nil) when the path is not indexed.
func (s *Store) NodeByPath(owner, p string) (*Node, error) { return nodeAt(s.db, owner, p) }

func (s *Store) NodeByID(id int64) (*Node, error) {
	n := &Node{}
	var d int
	err := s.db.QueryRow(`SELECT `+nodeCols+`,owner FROM nodes WHERE id=?`, id).
		Scan(&n.ID, &n.Path, &n.Dir, &d, &n.Size, &n.Mtime, &n.ETag, &n.Owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	n.IsDir = d == 1
	return n, err
}

func (s *Store) NodeExists(id int64) bool {
	var x int
	return s.db.QueryRow(`SELECT 1 FROM nodes WHERE id=?`, id).Scan(&x) == nil
}

type execer interface {
	Exec(string, ...any) (sql.Result, error)
}

func touch(tx execer, owner string, paths []string) error {
	etag := randHex(8)
	for _, p := range paths {
		if _, err := tx.Exec(`UPDATE nodes SET etag=? WHERE owner=? AND path=?`, etag, owner, p); err != nil {
			return err
		}
	}
	return nil
}

func deleteTree(tx execer, owner, p string) error {
	for _, tbl := range []string{"favorites", "album_items"} {
		if _, err := tx.Exec(`DELETE FROM `+tbl+` WHERE node_id IN (SELECT id FROM nodes WHERE owner=? AND path=?)`, owner, p); err != nil {
			return err
		}
		if p != "" {
			if _, err := tx.Exec(`DELETE FROM `+tbl+` WHERE node_id IN (SELECT id FROM nodes WHERE owner=? AND path>=? AND path<?)`,
				owner, p+"/", p+"0"); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`DELETE FROM nodes WHERE owner=? AND path=?`, owner, p); err != nil {
		return err
	}
	if p == "" {
		return nil
	}
	// '0' is the character right after '/', so this is exactly the subtree "p/..."
	_, err := tx.Exec(`DELETE FROM nodes WHERE owner=? AND path>=? AND path<?`, owner, p+"/", p+"0")
	return err
}

func fiAttrs(fi fs.FileInfo) (isDir bool, size, mtime int64) {
	isDir = fi.IsDir()
	if !isDir {
		size = fi.Size()
	}
	return isDir, size, fi.ModTime().Unix()
}

// SyncPath upserts one path from its FileInfo. Ancestor etags change when
// something was created or modified. Returns the node.
func (s *Store) SyncPath(owner, p string, fi fs.FileInfo) (*Node, error) {
	if badPath(p) {
		return nil, errBadPath
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	n, err := nodeAt(tx, owner, p)
	if err != nil {
		return nil, err
	}
	isDir, size, mt := fiAttrs(fi)
	bump := false
	switch {
	case n == nil:
		n = &Node{Owner: owner, Path: p, Dir: ParentOf(p), IsDir: isDir, Size: size, Mtime: mt, ETag: randHex(8)}
		res, err := tx.Exec(`INSERT INTO nodes(owner,path,dir,is_dir,size,mtime,etag) VALUES(?,?,?,?,?,?,?)`,
			owner, p, n.Dir, b2i(isDir), size, mt, n.ETag)
		if err != nil {
			return nil, err
		}
		n.ID, _ = res.LastInsertId()
		bump = true
	case n.IsDir != isDir || (!isDir && (n.Size != size || n.Mtime != mt)):
		n.IsDir, n.Size, n.Mtime, n.ETag = isDir, size, mt, randHex(8)
		if _, err := tx.Exec(`UPDATE nodes SET is_dir=?,size=?,mtime=?,etag=? WHERE id=?`,
			b2i(isDir), size, mt, n.ETag, n.ID); err != nil {
			return nil, err
		}
		bump = true
	case isDir && n.Mtime != mt:
		n.Mtime = mt // directory mtime alone is not a content change
		if _, err := tx.Exec(`UPDATE nodes SET mtime=? WHERE id=?`, mt, n.ID); err != nil {
			return nil, err
		}
	}
	if bump {
		if err := touch(tx, owner, ancestors(p)); err != nil {
			return nil, err
		}
	}
	return n, tx.Commit()
}

// ReconcileDir brings the index of one directory in line with what is on disk
// (insert new, update changed, drop vanished) in a single transaction and
// returns the child nodes. If anything changed, the directory's own etag and
// all of its ancestors' etags change too.
//
// infos is a snapshot of the disk taken a moment earlier, and a big directory takes
// seconds to read. onDisk (may be nil) re-checks single paths at apply time, so a
// stale snapshot can neither delete the record of a file that has just been created
// nor insert one for a file that is already gone. ErrStale is returned when the
// directory itself was moved or deleted in the meantime: nothing is written and the
// caller reads the disk again. Applying such a snapshot anyway would index the old
// contents under the old path while the real records already moved to the new one,
// counting every file twice.
func (s *Store) ReconcileDir(owner, dir string, infos []fs.FileInfo, onDisk func(rel string) bool) ([]*Node, error) {
	if badPath(dir) {
		return nil, errBadPath
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if dir != "" {
		if dn, err := nodeAt(tx, owner, dir); err != nil {
			return nil, err
		} else if dn == nil || !dn.IsDir || (onDisk != nil && !onDisk(dir)) {
			return nil, ErrStale
		}
	}

	rows, err := tx.Query(`SELECT `+nodeCols+` FROM nodes WHERE owner=? AND dir=? AND path<>''`, owner, dir)
	if err != nil {
		return nil, err
	}
	known := map[string]*Node{}
	for rows.Next() {
		n := &Node{Owner: owner}
		var d int
		if err := rows.Scan(&n.ID, &n.Path, &n.Dir, &d, &n.Size, &n.Mtime, &n.ETag); err != nil {
			rows.Close()
			return nil, err
		}
		n.IsDir = d == 1
		known[n.Path] = n
	}
	rows.Close()

	changed := false
	seen := make(map[string]struct{}, len(infos))
	out := make([]*Node, 0, len(infos))
	for _, fi := range infos {
		p := Join(dir, fi.Name())
		seen[p] = struct{}{}
		isDir, size, mt := fiAttrs(fi)
		n := known[p]
		if n == nil && onDisk != nil && !onDisk(p) {
			continue // already gone again: do not index a phantom
		}
		switch {
		case n == nil:
			n = &Node{Owner: owner, Path: p, Dir: dir, IsDir: isDir, Size: size, Mtime: mt, ETag: randHex(8)}
			res, err := tx.Exec(`INSERT INTO nodes(owner,path,dir,is_dir,size,mtime,etag) VALUES(?,?,?,?,?,?,?)`,
				owner, p, dir, b2i(isDir), size, mt, n.ETag)
			if err != nil {
				return nil, err
			}
			n.ID, _ = res.LastInsertId()
			changed = true
		case n.IsDir != isDir || (!isDir && (n.Size != size || n.Mtime != mt)):
			n.IsDir, n.Size, n.Mtime, n.ETag = isDir, size, mt, randHex(8)
			if _, err := tx.Exec(`UPDATE nodes SET is_dir=?,size=?,mtime=?,etag=? WHERE id=?`,
				b2i(isDir), size, mt, n.ETag, n.ID); err != nil {
				return nil, err
			}
			changed = true
		case isDir && n.Mtime != mt:
			n.Mtime = mt
			if _, err := tx.Exec(`UPDATE nodes SET mtime=? WHERE id=?`, mt, n.ID); err != nil {
				return nil, err
			}
		}
		out = append(out, n)
	}
	for p := range known {
		if _, ok := seen[p]; !ok {
			if onDisk != nil && onDisk(p) {
				continue // created after the snapshot was taken: keep its record (and its file id)
			}
			if err := deleteTree(tx, owner, p); err != nil {
				return nil, err
			}
			changed = true
		}
	}
	if changed {
		if err := touch(tx, owner, append([]string{dir}, ancestors(dir)...)); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

// DeletePath drops a path (and everything below it) from the index.
func (s *Store) DeletePath(owner, p string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteTree(tx, owner, p); err != nil {
		return err
	}
	if err := touch(tx, owner, ancestors(p)); err != nil {
		return err
	}
	return tx.Commit()
}

// MovePath re-keys a node and its subtree. File ids are preserved.
func (s *Store) MovePath(owner, from, to string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteTree(tx, owner, to); err != nil {
		return err
	}
	type row struct {
		id int64
		p  string
	}
	var moving []row
	rows, err := tx.Query(`SELECT id,path FROM nodes WHERE owner=? AND (path=? OR (path>=? AND path<?))`,
		owner, from, from+"/", from+"0")
	if err != nil {
		return err
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.p); err != nil {
			rows.Close()
			return err
		}
		moving = append(moving, r)
	}
	rows.Close()
	for _, r := range moving {
		np := to + strings.TrimPrefix(r.p, from)
		if _, err := tx.Exec(`UPDATE nodes SET path=?,dir=? WHERE id=?`, np, ParentOf(np), r.id); err != nil {
			return err
		}
	}
	if err := touch(tx, owner, ancestors(from)); err != nil {
		return err
	}
	if err := touch(tx, owner, ancestors(to)); err != nil {
		return err
	}
	return tx.Commit()
}

// DirSize sums the indexed files below p ("" = whole account).
func (s *Store) DirSize(owner, p string) (n int64) {
	if p == "" {
		s.db.QueryRow(`SELECT COALESCE(SUM(size),0) FROM nodes WHERE owner=? AND is_dir=0`, owner).Scan(&n)
		return
	}
	s.db.QueryRow(`SELECT COALESCE(SUM(size),0) FROM nodes WHERE owner=? AND is_dir=0 AND path>=? AND path<?`,
		owner, p+"/", p+"0").Scan(&n)
	return
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
