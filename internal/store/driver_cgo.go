//go:build cgo_sqlite

package store

// Alternative driver, for building natively on the router with Entware gcc:
//   CGO_ENABLED=1 go build -tags cgo_sqlite ./cmd/nclite
import _ "github.com/mattn/go-sqlite3"

const driverName = "sqlite3"

func dsn(path string) string {
	return "file:" + path + "?_busy_timeout=10000&_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=1&_txlock=immediate"
}
