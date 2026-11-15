package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---- favorites (per user, keyed by file id so they survive renames and trash) -------

func (s *Store) SetFavorite(user string, nodeID int64, fav bool) error {
	var err error
	if fav {
		_, err = s.db.Exec(`INSERT OR IGNORE INTO favorites(user,node_id) VALUES(?,?)`, user, nodeID)
	} else {
		_, err = s.db.Exec(`DELETE FROM favorites WHERE user=? AND node_id=?`, user, nodeID)
	}
	return err
}

func (s *Store) FavoriteSet(user string) map[int64]bool {
	out := map[int64]bool{}
	rows, err := s.db.Query(`SELECT node_id FROM favorites WHERE user=?`, user)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// FavoriteNodes returns the indexed nodes the user marked as favorite.
func (s *Store) FavoriteNodes(user string) ([]*Node, error) {
	rows, err := s.db.Query(`SELECT n.id,n.owner,n.path,n.dir,n.is_dir,n.size,n.mtime,n.etag
		FROM favorites f JOIN nodes n ON n.id=f.node_id WHERE f.user=?`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n := &Node{}
		var d int
		if err := rows.Scan(&n.ID, &n.Owner, &n.Path, &n.Dir, &d, &n.Size, &n.Mtime, &n.ETag); err != nil {
			return nil, err
		}
		n.IsDir = d == 1
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---- trash ---------------------------------------------------------------------------

// TrashOwner is the pseudo owner under which trashed index rows live. User names
// cannot contain '~', so it never collides with a real account.
func TrashOwner(user string) string { return "~trash:" + user }

type TrashItem struct {
	ID        int64
	Owner     string
	Name      string
	OrigDir   string
	DeletedAt int64
	IsDir     bool
	Size      int64
}

// ItemName is the name the item has in the trash collection ("a.jpg.d1700000000").
func (t TrashItem) ItemName() string { return fmt.Sprintf("%s.d%d", t.Name, t.DeletedAt) }
func (t TrashItem) OrigPath() string { return Join(t.OrigDir, t.Name) }

func (s *Store) TrashAdd(owner, name, origDir string, isDir bool, size int64) (TrashItem, error) {
	at := time.Now().Unix()
	for {
		res, err := s.db.Exec(`INSERT OR IGNORE INTO trash(owner,name,orig_dir,deleted_at,is_dir,size) VALUES(?,?,?,?,?,?)`,
			owner, name, origDir, at, b2i(isDir), size)
		if err != nil {
			return TrashItem{}, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			id, _ := res.LastInsertId()
			return TrashItem{ID: id, Owner: owner, Name: name, OrigDir: origDir, DeletedAt: at, IsDir: isDir, Size: size}, nil
		}
		at++ // same name deleted twice within a second
	}
}

const trashCols = `id,owner,name,orig_dir,deleted_at,is_dir,size`

func scanTrash(sc interface{ Scan(...any) error }) (TrashItem, error) {
	var t TrashItem
	var d int
	err := sc.Scan(&t.ID, &t.Owner, &t.Name, &t.OrigDir, &t.DeletedAt, &d, &t.Size)
	t.IsDir = d == 1
	return t, err
}

func (s *Store) trashQuery(q string, args ...any) ([]TrashItem, error) {
	rows, err := s.db.Query(`SELECT `+trashCols+` FROM trash `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrashItem
	for rows.Next() {
		t, err := scanTrash(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) TrashList(owner string) ([]TrashItem, error) {
	return s.trashQuery(`WHERE owner=? ORDER BY deleted_at DESC, id DESC`, owner)
}

func (s *Store) TrashOlderThan(ts int64) ([]TrashItem, error) {
	return s.trashQuery(`WHERE deleted_at<?`, ts)
}

// TrashByID returns (nil, nil) for an unknown id.
func (s *Store) TrashByID(id int64) (*TrashItem, error) {
	t, err := scanTrash(s.db.QueryRow(`SELECT `+trashCols+` FROM trash WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

// TrashByName resolves "<name>.d<unixtime>" for an owner; (nil, nil) if not found.
func (s *Store) TrashByName(owner, item string) (*TrashItem, error) {
	i := strings.LastIndex(item, ".d")
	if i <= 0 {
		return nil, nil
	}
	at, err := strconv.ParseInt(item[i+2:], 10, 64)
	if err != nil {
		return nil, nil
	}
	t, err := scanTrash(s.db.QueryRow(`SELECT `+trashCols+` FROM trash WHERE owner=? AND name=? AND deleted_at=?`,
		owner, item[:i], at))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

func (s *Store) TrashDelete(id int64) error {
	_, err := s.db.Exec(`DELETE FROM trash WHERE id=?`, id)
	return err
}

// RekeyTree moves an index subtree to another owner/path, keeping file ids. It is
// how files enter the trash (owner -> TrashOwner) and come back.
func (s *Store) RekeyTree(fromOwner, from, toOwner, to string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteTree(tx, toOwner, to); err != nil { // stale rows at the destination
		return err
	}
	type row struct {
		id int64
		p  string
	}
	var moving []row
	rows, err := tx.Query(`SELECT id,path FROM nodes WHERE owner=? AND (path=? OR (path>=? AND path<?))`,
		fromOwner, from, from+"/", from+"0")
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
		if _, err := tx.Exec(`UPDATE nodes SET owner=?,path=?,dir=? WHERE id=?`, toOwner, np, ParentOf(np), r.id); err != nil {
			return err
		}
	}
	if err := touch(tx, fromOwner, ancestors(from)); err != nil {
		return err
	}
	if err := touch(tx, toOwner, ancestors(to)); err != nil {
		return err
	}
	return tx.Commit()
}

// PurgeTree drops index rows (and their favorites) without touching any etags.
func (s *Store) PurgeTree(owner, p string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteTree(tx, owner, p); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- direct download links --------------------------------------------------------------

func (s *Store) AddDirect(token, user string, nodeID int64, expires time.Time) error {
	_, err := s.db.Exec(`INSERT INTO direct(token,user,node_id,expires) VALUES(?,?,?,?)`, token, user, nodeID, expires.Unix())
	return err
}

func (s *Store) GetDirect(token string) (user string, nodeID int64, ok bool) {
	var exp int64
	if s.db.QueryRow(`SELECT user,node_id,expires FROM direct WHERE token=?`, token).Scan(&user, &nodeID, &exp) != nil {
		return "", 0, false
	}
	return user, nodeID, time.Now().Unix() < exp
}

func (s *Store) GCDirect() {
	s.db.Exec(`DELETE FROM direct WHERE expires<?`, time.Now().Unix())
}

// ---- statistics and small key/value settings ---------------------------------------

// UserStats counts what the index knows about an account (exact after a full scan).
func (s *Store) UserStats(owner string) (files, dirs, bytes int64) {
	s.db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN is_dir=0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN is_dir=1 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN is_dir=0 THEN size ELSE 0 END),0)
		FROM nodes WHERE owner=? AND path<>''`, owner).Scan(&files, &dirs, &bytes)
	return
}

func (s *Store) TrashStats(owner string) (items, bytes int64) {
	s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size),0) FROM trash WHERE owner=?`, owner).Scan(&items, &bytes)
	return
}

func (s *Store) GetMeta(key string) string {
	var v string
	s.db.QueryRow(`SELECT v FROM meta WHERE k=?`, key).Scan(&v)
	return v
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, key, value)
	return err
}

// PathNoCase returns the indexed spelling of dir/name, matching letter case
// ignoring ASCII case (for case-insensitive disks); "" if nothing matches.
func (s *Store) PathNoCase(owner, dir, name string) string {
	var p string
	s.db.QueryRow(`SELECT path FROM nodes WHERE owner=? AND dir=? AND path=? COLLATE NOCASE LIMIT 1`,
		owner, dir, Join(dir, name)).Scan(&p)
	return p
}

// PurgeBadPaths removes index rows whose path is not canonical ("Photos/", "Photos//a.jpg").
// Older versions created them when a shared folder was opened by the person it was
// shared with, which indexed the owner's whole folder a second time under a new id.
// They are only duplicates: the real rows live under the proper path.
func (s *Store) PurgeBadPaths() (int64, error) {
	const bad = `path<>'' AND (substr(path,1,1)='/' OR substr(path,-1)='/' OR instr(path,'//')>0)`
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM favorites WHERE node_id IN (SELECT id FROM nodes WHERE ` + bad + `)`); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM nodes WHERE ` + bad)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}
