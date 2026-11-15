// nclite: a tiny Nextcloud-compatible file server for routers and other small boxes.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"nclite/internal/config"
	"nclite/internal/preview"
	"nclite/internal/server"
	"nclite/internal/store"
	"nclite/internal/vfs"
)

const usage = `usage: nclite [-config file.json] <command>

  serve                          run the server (default)
  user add <name> [password]     create an admin user (password from NCLITE_PASSWORD, argument or stdin)
  user passwd <name> [password]  change a password
  user list                      list users
`

func main() {
	cfgPath := flag.String("config", "/opt/etc/nclite/config.json", "config file (JSON); missing file = defaults")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		args = []string{"serve"}
	}
	if err := run(*cfgPath, args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(cfgPath string, args []string) error {
	path := cfgPath
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		path = "" // run on defaults
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	for _, d := range []string{filepath.Dir(cfg.DBPath), cfg.DataDir, cfg.CacheDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	switch args[0] {
	case "serve":
		return serve(cfg, st)
	case "user":
		return userCmd(cfg, st, args[1:])
	}
	fmt.Fprint(os.Stderr, usage)
	return errors.New("unknown command " + args[0])
}

func serve(cfg *config.Config, st *store.Store) error {
	if cfg.MemLimitMB > 0 {
		debug.SetMemoryLimit(int64(cfg.MemLimitMB) << 20)
	}
	if cfg.GCPercent > 0 {
		debug.SetGCPercent(cfg.GCPercent)
	}
	v, err := vfs.New(cfg.DataDir, st)
	if err != nil {
		return err
	}
	pv := preview.New(cfg.Preview, cfg.CacheDir)
	srv, err := server.New(cfg, st, v, pv)
	if err != nil {
		return err
	}
	if st.CountUsers() == 0 {
		slog.Warn("no users yet: create the first admin with `nclite user add <name>`")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go srv.Maintain(ctx)

	hs := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// no Read/WriteTimeout: uploads and downloads of large videos can take long
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	slog.Info("listening", "addr", cfg.Listen, "data", cfg.DataDir, "db", cfg.DBPath)
	adminDone := make(chan struct{})
	if cfg.AdminListen != "" {
		go func() { defer close(adminDone); serveAdmin(ctx, cfg, srv) }()
		slog.Info("admin UI on its own listener; it is NOT served on the main port",
			"listen", cfg.AdminListen, "path", cfg.AdminPrefix(), "url", cfg.AdminURL)
	} else {
		close(adminDone)
		slog.Warn("admin UI is served on the main listener: anyone who can reach it can try to log in. "+
			"Set admin_listen to keep it off the internet", "path", cfg.AdminPrefix())
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	<-adminDone
	return hs.Shutdown(sctx)
}

// serveAdmin runs the admin-only listener until ctx ends. If the address cannot be
// bound (on a router the LAN address may not be up yet at boot) it logs loudly and
// keeps retrying, instead of taking the whole server down or silently having no admin.
func serveAdmin(ctx context.Context, cfg *config.Config, srv *server.Server) {
	for ctx.Err() == nil {
		as := &http.Server{
			Addr:              cfg.AdminListen,
			Handler:           srv.AdminHandler(),
			ReadHeaderTimeout: 15 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		errc := make(chan error, 1)
		go func() { errc <- as.ListenAndServe() }()
		select {
		case err := <-errc:
			slog.Error("admin listener failed, retrying in 10s", "listen", cfg.AdminListen, "err", err)
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			as.Shutdown(sctx)
			cancel()
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
	}
}

func readPassword(args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	if p := os.Getenv("NCLITE_PASSWORD"); p != "" {
		return p, nil
	}
	fmt.Fprint(os.Stderr, "password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func userCmd(cfg *config.Config, st *store.Store, a []string) error {
	if len(a) == 0 {
		return errors.New("user: need add|passwd|list")
	}
	switch a[0] {
	case "list":
		us, err := st.ListUsers()
		for _, u := range us {
			role := "user"
			if u.Admin {
				role = "admin"
			}
			fmt.Printf("%-20s %s\n", u.Name, role)
		}
		return err
	case "add", "passwd":
		if len(a) < 2 {
			return errors.New("user " + a[0] + ": need a name")
		}
		pw, err := readPassword(a[2:])
		if err != nil {
			return err
		}
		if len(pw) < 8 {
			return errors.New("password must be at least 8 characters")
		}
		if a[0] == "passwd" {
			return st.SetPassword(a[1], server.HashPassword(pw))
		}
		if err := st.CreateUser(a[1], server.HashPassword(pw), true); err != nil {
			return err
		}
		v, _ := vfs.New(cfg.DataDir, st)
		return v.EnsureUser(a[1])
	}
	return errors.New("user: unknown subcommand " + a[0])
}
