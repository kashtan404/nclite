//go:build !cgo_sqlite

package store

import _ "modernc.org/sqlite" // pure-Go SQLite, no cgo

const driverName = "sqlite"

func dsn(path string) string {
	return "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_txlock=immediate"
}
