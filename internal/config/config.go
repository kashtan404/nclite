// Package config loads the JSON configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Preview struct {
	Enabled     bool   `json:"enabled"`
	Workers     int    `json:"workers"`      // parallel renderers (1 is right for a 2-core router)
	Pregen      []int  `json:"pregen"`       // square sizes rendered in background after upload
	FFmpeg      string `json:"ffmpeg"`       // images + video
	HeifConvert string `json:"heif_convert"` // libheif example tool, HEIC/HEIF
	Pdftoppm    string `json:"pdftoppm"`     // poppler-utils, PDF
	Nice        int    `json:"nice"`         // 0 disables `nice`
	TimeoutSec  int    `json:"timeout_sec"`
	Quality     int    `json:"quality"` // ffmpeg mjpeg -q:v (2 best .. 31 worst)
	// ExcludeExt lists file extensions that never get a preview (".pdf" or "pdf", any
	// letter case). The app then shows its generic icon for them and nothing is rendered.
	ExcludeExt []string `json:"exclude_ext"`
}

type Config struct {
	Listen        string  `json:"listen"`
	BaseURL       string  `json:"base_url"`    // public URL, e.g. https://cloud.example.com
	TrustProxy    bool    `json:"trust_proxy"` // honour X-Forwarded-* from nginx
	DBPath        string  `json:"db_path"`     // SQLite file (may live on another disk than data_dir)
	DataDir       string  `json:"data_dir"`    // user files: <data_dir>/<user>/files
	CacheDir      string  `json:"cache_dir"`   // preview cache
	ProductName   string  `json:"product_name"`
	MemLimitMB    int     `json:"mem_limit_mb"`
	GCPercent     int     `json:"gc_percent"`
	RescanMinutes int     `json:"rescan_minutes"` // 0 disables periodic rescan of out-of-band changes
	TrashDays     int     `json:"trash_days"`     // >0 purge after N days, 0 keep forever, -1 no trash (delete at once)
	AccessLog     bool    `json:"access_log"`
	AdminListen   string  `json:"admin_listen"` // dedicated listener for the admin UI only, e.g. "127.0.0.1:8090"
	AdminURL      string  `json:"admin_url"`    // where you open the admin, e.g. "http://192.168.1.1:83/admin"
	Preview       Preview `json:"preview"`
}

func Default() *Config {
	return &Config{
		Listen:        "127.0.0.1:8085",
		TrustProxy:    true,
		DBPath:        "/opt/var/nclite/nclite.db",
		DataDir:       "/opt/var/nclite/data",
		CacheDir:      "/opt/var/nclite/cache",
		ProductName:   "Home Cloud",
		MemLimitMB:    160,
		GCPercent:     50,
		RescanMinutes: 360,
		TrashDays:     30,
		Preview: Preview{
			Enabled:     true,
			Workers:     1,
			Pregen:      []int{512},
			FFmpeg:      "ffmpeg",
			HeifConvert: "heif-convert",
			Pdftoppm:    "pdftoppm",
			Nice:        10,
			TimeoutSec:  90,
			Quality:     5,
		},
	}
}

func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	c.AdminURL = strings.TrimRight(c.AdminURL, "/")
	if err := c.validateAdminURL(); err != nil {
		return nil, err
	}
	for _, p := range []*string{&c.DBPath, &c.DataDir, &c.CacheDir} {
		if *p == "" {
			return nil, errors.New("db_path, data_dir and cache_dir must be set")
		}
		*p = filepath.Clean(*p)
	}
	if c.Preview.Workers < 1 {
		c.Preview.Workers = 1
	}
	if c.Preview.Quality < 2 || c.Preview.Quality > 31 {
		c.Preview.Quality = 5
	}
	return c, nil
}

// reserved are the first path segments used by the Nextcloud API; the admin UI must not shadow them.
var reserved = map[string]bool{"remote.php": true, "ocs": true, "index.php": true, "login": true,
	"core": true, "apps": true, "status.php": true, "avatar": true}

func (c *Config) validateAdminURL() error {
	if c.AdminURL == "" {
		return nil
	}
	u, err := url.Parse(c.AdminURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("admin_url %q must look like http://192.168.1.1:83/admin", c.AdminURL)
	}
	p := c.AdminPrefix()
	if strings.Contains(p, "..") || reserved[strings.SplitN(strings.TrimPrefix(p, "/"), "/", 2)[0]] {
		return fmt.Errorf("admin_url path %q is not allowed (it would hide a Nextcloud API path)", p)
	}
	return nil
}

// AdminPrefix is the URL path the admin UI is served under: the path of admin_url, default "/admin".
func (c *Config) AdminPrefix() string {
	if u, err := url.Parse(c.AdminURL); err == nil {
		if p := strings.TrimRight(u.Path, "/"); p != "" {
			return p
		}
	}
	return "/admin"
}
