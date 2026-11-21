[![Build and publish nclite release](https://gitverse.ru/api/repos/kashtan404/nclite/actions/workflows/release.yml/badge.svg?lang=ru)](https://gitverse.ru/kashtan404/nclite/cicd?workflow=release.yml&workflowName=Build%20and%20publish%20nclite%20release)

# nclite

**A tiny, Nextcloud-compatible file server for routers and other small boxes.**

[Русская версия](README.md)

nclite implements the part of the Nextcloud server API that the official Nextcloud mobile app actually needs: browsing, uploading and downloading files, automatic photo upload, previews, trash, favorites, photo albums and video streaming. It is one static Go binary with no runtime dependencies besides optional media tools, and it was built to run on a consumer router (Keenetic, aarch64, Entware, 512 MB RAM) in front of a USB disk.

It is **not** a Nextcloud replacement: there are no apps, no calendar or contacts, no link sharing, no versioning. If you want your phone's photos and files on a disk you own, served through the app you already use, with a small footprint (the server process stays in the tens of megabytes), this is for you.

> nclite is an independent implementation written from the protocol up. It is not affiliated with or endorsed by Nextcloud GmbH. "Nextcloud" is a trademark of its owner; it is used here only to describe protocol compatibility.

## Contents

- [Features](#features)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Manual build](#manual-build)
- [Configuration](#configuration)
- [Deployment](#deployment)
- [Using it](#using-it)
- [Adopting an existing Nextcloud data directory](#adopting-an-existing-nextcloud-data-directory)
- [How it works](#how-it-works)
- [Security](#security)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)
- [Development](#development)
- [License](#license)

## Features

**For the phone app** (developed and tested with Nextcloud iOS 35.2)

- Log in with the browser flow (Login Flow v2); the app gets a revocable app password.
- Browse, upload, download, rename, move, copy and delete files and folders. Range requests, so video seeks.
- **Automatic photo upload** with original modification times, and chunked upload for large videos.
- Server-side **previews** for JPEG, PNG, GIF, WebP, HEIC/HEIF, video (mp4, mov, mkv and more) and PDF, generated once, cached on disk and served like the original server does.
- **Trash** with restore, permanent delete and automatic expiry. **Favorites.**
- **Video streaming** through direct-download links.
- **Photo albums** (the Nextcloud Photos protocol). Albums only list files, nothing is copied.
- **Shared folders** between users, read-only or read-write.
- Quota display from the real disk usage.

**For you**

- **Admin page**: users, per-user folder statistics (files, folders, size, trash), shared folders, scan and preview-generation jobs with live progress and cancel.
- **File browser** for your computer: folder tree, albums, list / table / 512 px / 1024 px views, a resizable viewer pane, copy / move / rename / delete on selected files. Served only on the admin port.
- Files live on disk **exactly as they are** (`<data_dir>/<user>/files/...`). Add, edit or remove files over scp or Samba and the server picks the change up. The layout matches a Nextcloud data directory, so you can [adopt one](#adopting-an-existing-nextcloud-data-directory) in place.
- The admin page and the file browser can be kept off the internet by giving them their own listener.

**Built for small hardware**

- Pure Go, no cgo by default, a single static binary. SQLite index in WAL mode.
- Memory is bounded (a soft limit for the Go runtime). Heavy work runs in short-lived external processes at low priority, one at a time, and returns its memory to the system when it exits.
- Previews are made from the smallest suitable existing preview instead of decoding the original again (a 12 MP HEIC is decoded once, the other sizes are scaled from the result).

## Requirements

- Linux. Developed and used on aarch64 (Keenetic with Entware); other architectures should build, but only aarch64 and amd64 were tried.
- A reverse proxy that terminates HTTPS (an nginx example is included). The iOS app expects HTTPS.
- Optional, for previews (install what you need; previews for other types still work):

  | Tool | Used for | Typical package |
  |---|---|---|
  | `ffmpeg` | images, video | `ffmpeg` |
  | `heif-convert` | HEIC / HEIF | libheif tools (`libheif-examples`) |
  | `pdftoppm` | PDF | `poppler-utils` |

- A current Go release to build from source.

## Quick start

```sh
# 1. Connect feed
cat > "/opt/etc/opkg/kashtan404-keenetic.conf" <<EOF
src/gz nclite https://kashtan404.gitverse.site/entware-repo/aarch64-k3.10
EOF

# 2. Install deps
opkg install ffmpeg poppler-utils python3 python3-pip nginx-ssl nginx-mod-headers-more
pip3 install heif-convert

# 3. Install nclite
opkg update
opkg install nclite

# 4. Configure /opt/etc/nclite/config.json
Set correct values for db_path, data_dir, cache_dir, listen, base_url, admin_listen, admin_url
Additionally you able to add excludes for extentions not need prefiew via set exclude_ext

# 5. Configure nginx
templates you can get from https://gitverse.ru/kashtan404/nclite/content/master/deploy
nginx.conf and conf.d/nclite.conf

# 6. Create the first user. The command-line tool creates administrators;
#    create ordinary users later on the admin page.
nclite -config /opt/etc/nclite/config.json user add alice

# 7. Start
/opt/etc/init.d/S99nclite start
/opt/etc/init.d/S80nginx start
```

## Manual build

```sh
# 1. Build (on any machine with Go)
go mod tidy
make build-arm64            # dist/nclite, static, no cgo. For other targets:
                            # GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dist/nclite ./cmd/nclite

# or download and unpack necessary release
https://gitverse.ru/kashtan404/nclite/releases

# 2. Install on the device (paths are the Entware defaults)
scp dist/nclite router:/opt/bin/
ssh router
mkdir -p /opt/etc/nclite
# copy deploy/config.example.json to /opt/etc/nclite/config.json and edit it:
# at least data_dir, cache_dir and base_url

# 3. Create the first user. The command-line tool creates administrators;
#    create ordinary users later on the admin page.
nclite user add alice

# 4. Run
nclite serve                # or install deploy/S99nclite as an init script
```

Then put nginx (or any proxy) with HTTPS in front of it, see [Deployment](#deployment), and enter your server address in the Nextcloud app. The app opens a login page served by nclite; sign in and you are connected.

## Configuration

A single JSON file, by default `/opt/etc/nclite/config.json` (`-config` to change it). A missing file means defaults. See [`deploy/config.example.json`](deploy/config.example.json).

| Key | Default | Meaning |
|---|---|---|
| `listen` | `127.0.0.1:8085` | Address of the main listener (the one behind your reverse proxy). |
| `base_url` | empty | Public address, e.g. `https://cloud.example.com`. Used in login-flow answers. If empty it is derived from the request headers. |
| `trust_proxy` | `true` | Honour `X-Forwarded-*` / `X-Real-IP`. Safe only when the listener is reachable solely from your proxy. |
| `data_dir` | `/opt/var/nclite/data` | User files: `<data_dir>/<user>/files`. Can be on a different disk than the database. |
| `db_path` | `/opt/var/nclite/nclite.db` | SQLite database (ids, etags, users, shares, albums). |
| `cache_dir` | `/opt/var/nclite/cache` | Preview cache. Safe to delete at any time. |
| `product_name` | `Home Cloud` | Name shown in the app and in the web pages. |
| `trash_days` | `30` | Purge trashed items after N days. `0` keeps them forever, `-1` disables the trash (deleted at once). |
| `rescan_minutes` | `360` | How often to rescan all users' folders for changes made outside nclite. `0` disables the periodic scan (accounts that were never scanned are still scanned once after start). |
| `mem_limit_mb` | `160` | Soft memory limit of the Go runtime. |
| `gc_percent` | `50` | Go GC target. Lower uses less memory and more CPU. |
| `access_log` | `false` | Log every request (useful to see what a client asks for). |
| `admin_listen` | empty | Address of a **separate listener for the admin page and the file browser**, e.g. `127.0.0.1:8090`. When set, neither is served on the main port. |
| `admin_url` | empty | The URL you open the admin page at, e.g. `http://192.168.1.1:83/admin`. Its path sets the admin prefix (default `/admin`); its scheme decides the `Secure` cookie flag. |
| `web_path` | `/cloud` | Path of the file browser on the admin listener. Needs `admin_listen`. |
| `preview.enabled` | `true` | Turn previews on or off. |
| `preview.workers` | `1` | Parallel preview renderers. Keep it at 1 on a small router. |
| `preview.pregen` | `[512]` | Square sizes rendered in the background after an upload. |
| `preview.ffmpeg` | `ffmpeg` | Path or name of the tool. |
| `preview.heif_convert` | `heif-convert` | Both the old (`in out.jpg`) and the new (`in -o out -f jpg`) command lines are supported. |
| `preview.pdftoppm` | `pdftoppm` | Path or name of the tool. |
| `preview.nice` | `10` | `nice` level for the tools; `0` disables it. |
| `preview.timeout_sec` | `90` | Kill a render that takes longer. |
| `preview.quality` | `5` | ffmpeg JPEG quality, `2` (best) to `31`. |
| `preview.exclude_ext` | `[]` | Extensions that never get a preview, e.g. `["pdf", ".avi"]`. The app then shows a generic icon. |

## Deployment

### Reverse proxy and HTTPS

[`deploy/nginx.conf`](deploy/nginx.conf) has a ready server block. The settings that matter for a file server:

```nginx
client_max_body_size 0;
proxy_request_buffering off;   # uploads stream through, nothing is spooled
proxy_buffering off;
proxy_http_version 1.1;
```

### Keeping the admin page off the internet

Set `admin_listen` to give the admin page and the file browser their own listener:

```json
"admin_listen": "127.0.0.1:8090",
"admin_url":    "http://192.168.1.1:83/admin"
```

The main port then answers 404 for `/admin` and `/cloud`, and the admin port serves nothing else. The example nginx config has a second `server` block that listens only on the router's LAN address and proxies to it. Do not bind `admin_listen` to `0.0.0.0` and do not forward that port from the internet. If the address is not available yet at boot, nclite keeps retrying and logs an error instead of exiting.

### Running as a service

[`deploy/S99nclite`](deploy/S99nclite) is an Entware init script (`start-stop-daemon`). Copy it to `/opt/etc/init.d/`. Logs go to `/opt/var/log/nclite.log`.

## Using it

**Connect the phone.** Enter the server URL in the Nextcloud app. It opens the nclite login page; after you sign in, the app stores an app password. "Sign out devices" on the admin page revokes them.

**Admin page** (`/admin` on the admin listener): create users and set passwords, see each user's folder, file count, size and trash, sign out their devices, delete an account (see below), create shared folders, and run jobs:

- *Scan files* walks the folders and updates the index. It makes the numbers exact and picks up files added outside nclite.
- *Generate previews* renders thumbnails ahead of time for all users, one user or one folder, with progress, estimated time and cancel. Sizes are processed largest first so the smaller ones are cheap. The form is pre-filled with the sizes your apps actually request.

**Deleting an account never deletes files.** The folder stays on disk and shows up under "Folders without an account"; creating an account with the same name connects it again.

**Shared folders.** An administrator shares a folder of one user with another, read-only or read-write. It appears in the recipient's root as `Folder (owner)` and keeps its file ids, so previews and favorites carry over.

**File browser** (`/cloud/` on the admin listener): sign in with your own name and password and you see what the app sees. Folder tree on the left with your albums below it; copy, move, rename, delete and new-folder actions on top for the files you tick; list, table, small (512 px) and large (1024 px) views; a viewer pane you can toggle and resize that shows photos (including HEIC), video, audio, PDF and text.

**Albums.** Create them in the app's Photos / Albums tab. An album lists files by id, so renaming or moving a photo does not break it, and deleting an album never touches the photos.

**Trash.** Deleting from the app moves the item to a trash folder next to your files (an instant rename). Restore it, delete it permanently or empty the trash from the app.

**Command line**

```
nclite [-config file.json] serve
nclite [-config file.json] user add <name> [password]     # creates an administrator
nclite [-config file.json] user passwd <name> [password]
nclite [-config file.json] user list
```

The password can also come from `NCLITE_PASSWORD` or stdin.

## Adopting an existing Nextcloud data directory

The layout is the same (`<data>/<user>/files/`), so you can point `data_dir` at a Nextcloud data directory without copying anything. nclite ignores everything it does not know (`appdata_*`, logs, `cache`, `files_trashbin`, `files_versions`, ...) and adds its own `tmp` and `trash` folders inside each user folder.

1. Back up first, and stop Nextcloud. Do not run both on the same directory with write access.
2. Open the admin page. "Folders without an account" lists the folders it found.
3. Create an account with the same name and a new password (Nextcloud passwords cannot be imported). Indexing of the existing files starts automatically; progress is shown on the admin page.

Carried over: your files. Not carried over: file ids, favorites, shares, comments, versions, the old trash, Nextcloud's previews. Sign in again on every device.

## How it works

```
<data_dir>/<user>/files/   your files, exactly as they are
<data_dir>/<user>/trash/   trashed items (same filesystem: moving in is a rename)
<data_dir>/<user>/tmp/     uploads in progress
<cache_dir>/<fileid>/...   preview cache
<db_path>                  SQLite index: file ids, etags, users, app passwords, shares, albums, favorites
```

- **Index.** Files are never stored in the database. SQLite only assigns stable file ids and etags and is kept in sync lazily: a folder is reconciled with the disk whenever it is listed, plus the periodic scan. Folder etags change when anything inside changes, all the way up to the root, which is how clients know what to refresh.
- **Consistency.** Renames, trash, restore, upload completion and folder creation are ordered against directory listings, and a listing re-checks the disk before it writes, so a slow listing cannot index a stale snapshot. Paths are always canonical; on case-insensitive disks (FAT, exFAT) requests are matched to the spelling on disk. If a user's disk is not mounted, nclite answers 503 instead of recreating empty folders and "forgetting" everything.
- **Previews.** Rendered by `ffmpeg` / `heif-convert` / `pdftoppm` as child processes under `nice`, with shrink-on-load for JPEG and EXIF orientation. Results are cached per file id and version. A smaller preview is derived from an existing larger one when that is safe; otherwise it is rendered from the original.

**Protocol coverage**

| Area | Endpoints |
|---|---|
| Status, login | `status.php`, `ocs/v{1,2}.php/cloud/capabilities`, `cloud/user`, `cloud/users/<id>`, Login Flow v2, app-password revoke |
| Files | WebDAV `/remote.php/dav/files/<user>`: PROPFIND, GET (Range), PUT (`X-OC-Mtime`), MKCOL, DELETE, MOVE, COPY, PROPPATCH (favorites), REPORT (favorites list) |
| Upload | Chunked upload v2 `/remote.php/dav/uploads/<user>` |
| Trash | `/remote.php/dav/trashbin/<user>`: list, restore, delete, empty, previews |
| Previews | `index.php/core/preview` (by id or path), `preview.png` |
| Video | `ocs/v2.php/apps/dav/api/v1/direct` and `/remote.php/direct/<token>` |
| Albums | `/remote.php/dav/photos/<user>/albums`, `COPY` into an album, `SEARCH` by file id |

Anything else under `ocs/` answers with an empty, valid OCS reply so clients do not break on features nclite does not have.

## Security

- Passwords are hashed with PBKDF2-HMAC-SHA256 (used only for interactive logins). Devices use random 256-bit app passwords stored as SHA-256, so authenticating a request is cheap. Failed logins are throttled per IP.
- Admin and file-browser sessions are signed cookies limited to their own path, and every state-changing request needs a CSRF token. The admin page runs no scripts at all; the file browser allows scripts and styles only from its own origin and renders file names as text.
- The file browser shows only photos, video, audio, PDF and plain text inline. HTML, SVG and everything else is served as a download only, so a file in a shared folder cannot run as a page of the site.
- No code path deletes a user's folder. Deleting the root of an account or of a shared folder is refused.
- Terminate TLS in the proxy, keep `listen` on a loopback address, keep `admin_listen` on the LAN, and leave `trust_proxy` on only when the proxy is the only thing that can reach `listen`.

## Limitations

- Only the features listed above. No link sharing or sharing API (so the app's "Details" screen is hidden), no activity, notifications, comments, file versions, end-to-end encryption, search, per-user quotas, or shared albums.
- Shares are created by an administrator, not by users.
- Tested with the Nextcloud iOS app. The Android and desktop clients were not tested.
- Counts and sizes are exact after a full scan; until then they cover only what nclite has seen.
- On case-insensitive disks two names that differ only in letter case cannot coexist, as on Windows.
- Moving or copying a file between accounts (into or out of a shared folder) really copies the data and gives the new file a new id.

## Troubleshooting

| Symptom | Check |
|---|---|
| No previews at all | The tools must be in the service's `PATH` (or set the full path in the config). Run the same command by hand. Turn on `"access_log": true` and read the log. |
| No HEIC previews | `heif-convert` needs an HEVC decoder (for example the libde265 plugin). |
| HTTP 503 "storage unavailable" | The data disk is not mounted. nclite refuses to touch an empty mount point; mount the disk and retry. |
| Upload fails behind nginx | Set `client_max_body_size 0;` and `proxy_request_buffering off;`. |
| App refuses the server | It needs HTTPS and a certificate the phone trusts. |
| SQLite fails to start on an old kernel | Build natively with `make build-cgo` (uses `mattn/go-sqlite3`, needs gcc). |
| Numbers look low after adding files by hand | Run "Scan files" on the admin page. |

## Development

```sh
make test           # go test -race (uses the cgo SQLite driver under a build tag)
make build-arm64    # static linux/arm64 binary, pure Go
make build-cgo      # native build with the cgo SQLite driver
```

Layout:

```
cmd/nclite        entry point and command-line tool
internal/config   config file
internal/store    SQLite: users, index, shares, albums, trash, favorites
internal/vfs      path resolution, shares, directory reconciliation, case handling
internal/preview  preview rendering and cache
internal/server   HTTP: Nextcloud API, WebDAV, admin page, file browser (static/, templates/)
deploy/           example config, nginx config, init script
```

The default driver is `modernc.org/sqlite` (pure Go). Tests run with the `cgo_sqlite` tag, which swaps in `mattn/go-sqlite3`.

## License

[GPLv3](LICENSE)
