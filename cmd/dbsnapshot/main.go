// Command dbsnapshot makes a consistent single-file copy of a SQLite database:
//
//	go run ./cmd/dbsnapshot -db eve-empire.db -out dist/deploy-package/data/eve-empire.db
//
// VACUUM INTO folds the WAL into the copy and takes a read lock for the
// duration, so the result is consistent even while the server is running —
// unlike copying .db/-wal/-shm by hand, where the three files can be torn
// relative to each other. The copy is also compacted: no free pages.
//
// It exists for the same reason as cmd/dbq: modernc's driver needs a Go
// program to talk to SQLite, there is no sqlite3 CLI on this machine.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := flag.String("db", "eve-empire.db", "source database")
	out := flag.String("out", "", "destination file (must not exist)")
	flag.Parse()
	if *out == "" {
		log.Fatal("-out is required")
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatal(err)
	}
	// VACUUM INTO refuses to overwrite, and that is the behaviour we want
	// for an accidental re-run; removing here is the explicit opt-in.
	if err := os.Remove(*out); err != nil && !os.IsNotExist(err) {
		log.Fatal(err)
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	abs, err := filepath.Abs(*out)
	if err != nil {
		log.Fatal(err)
	}
	// SQLite wants forward slashes even on Windows; ' doubles inside a literal.
	lit := strings.ReplaceAll(filepath.ToSlash(abs), "'", "''")
	if _, err := db.Exec("VACUUM INTO '" + lit + "'"); err != nil {
		log.Fatal(err)
	}

	src, _ := os.Stat(*dbPath)
	dst, err := os.Stat(abs)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s (%.1f МБ) -> %s (%.1f МБ)\n",
		*dbPath, float64(src.Size())/(1<<20), *out, float64(dst.Size())/(1<<20))
}
