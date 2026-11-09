// mysqlsmoke is a host-side real MySQL driver client; it is not shipped in the image.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"github.com/go-sql-driver/mysql"
	"log"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:3306", "address")
	query := flag.String("query", "SELECT 1", "SQL")
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
	rows, err := d.Query(*query)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
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
	if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
		log.Fatal(err)
	}
}
