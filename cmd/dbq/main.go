// Command dbq is a small read-only query helper for the static database:
//
//	go run ./cmd/dbq -db sde.db "SELECT name_en FROM types WHERE type_id = 1230"
//
// It exists because sde.db is the answer to most "where does the game
// keep this number" questions, and modernc's driver needs a Go program
// to ask — there is no sqlite3 CLI on this machine.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"strings"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := flag.String("db", "sde.db", "database")
	flag.Parse()
	db, err := sql.Open("sqlite", *dbPath+"?mode=ro")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(strings.Join(flag.Args(), " "))
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	fmt.Println(strings.Join(cols, "\t"))
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			log.Fatal(err)
		}
		out := make([]string, len(cols))
		for i, v := range vals {
			switch t := v.(type) {
			case nil:
				out[i] = "-"
			case []byte:
				out[i] = fmt.Sprintf("<%d bytes>", len(t))
			default:
				out[i] = fmt.Sprint(t)
			}
		}
		fmt.Println(strings.Join(out, "\t"))
	}
}
