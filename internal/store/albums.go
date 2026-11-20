package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Albums are per-user lists of files, kept by file id so they survive renames and
// moves (the Nextcloud Photos model). The files themselves are never copied.

var ErrExists = errors.New("already exists")

type Album struct {
	ID      int64
	Owner   string
	Name    string
	Created int64
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE")
}

func (s *Store) CreateAlbum(owner, name string) (*Album, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO albums(owner,name,created) VALUES(?,?,?)`, owner, name, now)
	if isUnique(err) {
		return nil, ErrExists
	}
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Album{ID: id, Owner: owner, Name: name, Created: now}, nil
}

func (s *Store) ListAlbums(owner string) ([]Album, error) {
	rows, err := s.db.Query(`SELECT id,owner,name,created FROM albums WHERE owner=? ORDER BY name COLLATE NOCASE, name`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Album
	for rows.Next() {
		var a Album
		if err := rows.Scan(&a.ID, &a.Owner, &a.Name, &a.Created); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AlbumByName / AlbumByID return (nil, nil) when there is no such album of this owner.
func (s *Store) AlbumByName(owner, name string) (*Album, error) {
	return s.album(`SELECT id,owner,name,created FROM albums WHERE owner=? AND name=?`, owner, name)
}

func (s *Store) AlbumByID(owner string, id int64) (*Album, error) {
	return s.album(`SELECT id,owner,name,created FROM albums WHERE owner=? AND id=?`, owner, id)
}

func (s *Store) album(q string, args ...any) (*Album, error) {
	var a Album
	err := s.db.QueryRow(q, args...).Scan(&a.ID, &a.Owner, &a.Name, &a.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

func (s *Store) RenameAlbum(id int64, to string) error {
	_, err := s.db.Exec(`UPDATE albums SET name=? WHERE id=?`, to, id)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func (s *Store) DeleteAlbum(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM album_items WHERE album_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM albums WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// AlbumAdd returns false if the file was already in the album.
func (s *Store) AlbumAdd(albumID, nodeID int64) (bool, error) {
	now := time.Now()
	res, err := s.db.Exec(`INSERT OR IGNORE INTO album_items(album_id,node_id,added,seq)
		VALUES(?,?,?, COALESCE((SELECT MAX(seq) FROM album_items WHERE album_id=?),0)+1)`,
		albumID, nodeID, now.Unix(), albumID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) AlbumRemove(albumID, nodeID int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM album_items WHERE album_id=? AND node_id=?`, albumID, nodeID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// AlbumItems returns the files of an album in the order they were added. Nodes can
// belong to any owner (files shared with the album's owner are allowed); callers
// check access.
func (s *Store) AlbumItems(albumID int64) ([]*Node, error) {
	rows, err := s.db.Query(`SELECT n.id,n.owner,n.path,n.dir,n.is_dir,n.size,n.mtime,n.etag
		FROM album_items i JOIN nodes n ON n.id=i.node_id WHERE i.album_id=? ORDER BY i.seq`, albumID)
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

// ChildDirs lists the indexed sub-directories of dir without touching the disk
// (used for the web UI folder tree, which must stay fast on huge folders).
func (s *Store) ChildDirs(owner, dir string) ([]*Node, error) {
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM nodes WHERE owner=? AND dir=? AND is_dir=1 AND path<>'' ORDER BY path COLLATE NOCASE`, owner, dir)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n := &Node{Owner: owner}
		var d int
		if err := rows.Scan(&n.ID, &n.Path, &n.Dir, &d, &n.Size, &n.Mtime, &n.ETag); err != nil {
			return nil, err
		}
		n.IsDir = true
		out = append(out, n)
	}
	return out, rows.Err()
}
