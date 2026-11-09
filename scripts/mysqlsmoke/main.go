// mysqlsmoke is a host-side real MySQL driver client; it is not shipped in the image.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"github.com/go-sql-driver/mysql"
	"log"
	"os"
	"strings"
	"time"
)

type queries []string

func (q *queries) String() string     { return strings.Join(*q, "; ") }
func (q *queries) Set(v string) error { *q = append(*q, v); return nil }

func main() {
	addr := flag.String("addr", "127.0.0.1:3306", "address")
	var statements queries
	flag.Var(&statements, "query", "SQL; repeat to execute statements on one connection")
	password := flag.String("password", "contest-password", "password")
	flag.Parse()
	cfg := mysql.NewConfig()
	cfg.User = "root"
	cfg.Passwd = *password
	cfg.Net = "tcp"
	cfg.Addr = *addr
	cfg.DBName = "contest"
	cfg.Timeout = 3 * time.Second
	cfg.ReadTimeout = 10 * time.Second
	cfg.WriteTimeout = 10 * time.Second
	d, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	if err = d.Ping(); err != nil {
		log.Fatal(err)
	}
	conn, err := d.Conn(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	if len(statements) == 0 {
		statements = queries{"SELECT 1"}
	}
	for _, query := range statements {
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "SELECT") {
			if _, err := conn.ExecContext(context.Background(), query); err != nil {
				log.Fatal(err)
			}
			continue
		}
		rows, err := conn.QueryContext(context.Background(), query)
		if err != nil {
			log.Fatal(err)
		}

		cols, err := rows.Columns()
		if err != nil {
			log.Fatal(err)
		}
		result := [][]string{}
		for rows.Next() {
			values := make([]string, len(cols))
			dest := make([]any, len(cols))
			for i := range values {
				dest[i] = &values[i]
			}
			if err = rows.Scan(dest...); err != nil {
				log.Fatal(err)
			}
			result = append(result, values)
		}
		if err = rows.Err(); err != nil {
			log.Fatal(err)
		}
		rows.Close()
		if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
			log.Fatal(err)
		}
	}

}
