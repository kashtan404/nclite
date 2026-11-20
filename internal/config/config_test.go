package config

import "testing"

func TestWebAndAdminPaths(t *testing.T) {
	c := Default()
	if c.WebPrefix() != "/cloud" || c.AdminPrefix() != "/admin" {
		t.Fatalf("defaults: %s %s", c.WebPrefix(), c.AdminPrefix())
	}
	c.WebPath = "files/"
	if c.WebPrefix() != "/files" {
		t.Fatalf("got %s", c.WebPrefix())
	}
	c.AdminURL = "http://192.168.1.1:83/panel/"
	if c.AdminPrefix() != "/panel" {
		t.Fatalf("got %s", c.AdminPrefix())
	}
	for _, bad := range []string{"/remote.php", "/ocs/x", "/panel", "/panel/files", "/", "/a/../b"} {
		c := Default()
		c.AdminURL = "http://h/panel"
		c.WebPath = bad
		err := c.validateWebPath()
		if bad == "/" { // empty falls back to the default, which is fine
			if err != nil {
				t.Errorf("web_path %q should fall back to /cloud: %v", bad, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("web_path %q must be rejected", bad)
		}
	}
	c = Default()
	c.AdminURL = "http://h/cloud" // admin on the path the file browser uses by default
	if c.validateWebPath() == nil {
		t.Error("overlapping admin and web paths must be rejected")
	}
}
