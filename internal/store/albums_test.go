package store

import "testing"

func TestAlbumsLifecycle(t *testing.T) {
	s := open(t)
	s.SyncPath("u", "", fi("x", true, 0))
	f1, _ := s.SyncPath("u", "a.jpg", fi("a", false, 1))
	f2, _ := s.SyncPath("u", "b.jpg", fi("b", false, 2))

	a, err := s.CreateAlbum("u", "Trip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAlbum("u", "Trip"); err != ErrExists {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := s.CreateAlbum("other", "Trip"); err != nil {
		t.Fatalf("the same name for another user must work: %v", err)
	}
	b, _ := s.CreateAlbum("u", "Zoo")
	if err := s.RenameAlbum(b.ID, "Trip"); err != ErrExists {
		t.Fatalf("rename onto an existing name: %v", err)
	}

	if ok, _ := s.AlbumAdd(a.ID, f1.ID); !ok {
		t.Fatal("first add must report true")
	}
	if ok, _ := s.AlbumAdd(a.ID, f1.ID); ok {
		t.Fatal("adding twice must report false")
	}
	s.AlbumAdd(a.ID, f2.ID)
	items, _ := s.AlbumItems(a.ID)
	if len(items) != 2 || items[0].ID != f1.ID || items[1].ID != f2.ID {
		t.Fatalf("items must come in the order they were added: %+v", items)
	}
	if got, _ := s.AlbumByID("other", a.ID); got != nil {
		t.Fatal("an album must not be reachable by another user")
	}

	// the album follows the file when it is renamed or moved ...
	s.SyncPath("u", "d", fi("d", true, 0))
	if err := s.MovePath("u", "a.jpg", "d/renamed.jpg"); err != nil {
		t.Fatal(err)
	}
	items, _ = s.AlbumItems(a.ID)
	if len(items) != 2 || items[0].Path != "d/renamed.jpg" {
		t.Fatalf("album must follow a moved file: %+v", items)
	}
	// ... survives the trash (rows are only re-keyed), and drops files that are gone for good
	if err := s.RekeyTree("u", "b.jpg", TrashOwner("u"), "1"); err != nil {
		t.Fatal(err)
	}
	if items, _ = s.AlbumItems(a.ID); len(items) != 2 {
		t.Fatal("a trashed file keeps its album membership so a restore brings it back")
	}
	if err := s.DeletePath("u", "d/renamed.jpg"); err != nil {
		t.Fatal(err)
	}
	if items, _ = s.AlbumItems(a.ID); len(items) != 1 {
		t.Fatalf("a deleted file must leave the album, %d items left", len(items))
	}
	if err := s.PurgeTree(TrashOwner("u"), "1"); err != nil {
		t.Fatal(err)
	}
	if items, _ = s.AlbumItems(a.ID); len(items) != 0 {
		t.Fatal("a purged trash item must leave the album")
	}

	if ok, _ := s.AlbumRemove(a.ID, f1.ID); ok {
		t.Fatal("removing something that is not there must report false")
	}
	if err := s.DeleteAlbum(a.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.AlbumByName("u", "Trip"); got != nil {
		t.Fatal("album should be gone")
	}
	if err := s.DeleteUser("u"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListAlbums("u"); len(list) != 0 {
		t.Fatal("deleting the user removes the albums")
	}
}

func TestChildDirsReadsOnlyTheIndex(t *testing.T) {
	s := open(t)
	s.SyncPath("u", "", fi("x", true, 0))
	for _, p := range []string{"b", "A", "A/x", "a2"} {
		s.SyncPath("u", p, fi("n", p != "A/x" && p != "a2", 1))
	}
	got, _ := s.ChildDirs("u", "")
	var names []string
	for _, n := range got {
		names = append(names, n.Path)
	}
	if len(names) != 2 || names[0] != "A" || names[1] != "b" {
		t.Fatalf("want the two folders sorted ignoring case, got %v", names)
	}
}
