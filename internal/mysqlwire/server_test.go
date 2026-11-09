package mysqlwire

import (
	"bytes"
	"database/sql"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"minivectordb/internal/db"
)

func TestDriverSession(t *testing.T) {
	e, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go Serve(c, e)
		}
	}()
	for _, password := range []string{"", "any-password"} {
		cfg := mysql.NewConfig()
		cfg.User = "contest"
		cfg.Passwd = password
		cfg.Net = "tcp"
		cfg.Addr = l.Addr().String()
		cfg.DBName = "ignored"
		cfg.ReadTimeout = time.Second
		cfg.WriteTimeout = time.Second
		d, err := sql.Open("mysql", cfg.FormatDSN())
		if err != nil {
			t.Fatal(err)
		}
		d.SetMaxOpenConns(1)
		func() {
			defer d.Close()
			if err := d.Ping(); err != nil {
				t.Fatal(err)
			}
			exec := func(q string, n int64) {
				t.Helper()
				r, err := d.Exec(q)
				if err != nil {
					t.Fatal(err)
				}
				got, err := r.RowsAffected()
				if err != nil || got != n {
					t.Fatalf("affected %d, %v; want %d", got, err, n)
				}
			}
			exec("CREATE TABLE items (id INT, label TEXT, v VECTOR(2))", 0)
			label := strings.Repeat("界", 100)
			exec("INSERT INTO items VALUES (1, '"+label+"', [1,0])", 1)
			if _, err := d.Exec("invalid SQL"); err == nil {
				t.Fatal("expected SQL error")
			}
			var id int
			var text, vector string
			if err := d.QueryRow("SELECT * FROM items ORDER BY DISTANCE(v, [1,0]) LIMIT 1").Scan(&id, &text, &vector); err != nil {
				t.Fatal(err)
			}
			if id != 1 || text != label || vector != "[1,0]" {
				t.Fatalf("unexpected row %d %q %q", id, text, vector)
			}
			exec("DELETE FROM items", 1)
			rows, err := d.Query("SELECT * FROM items")
			if err != nil {
				t.Fatal(err)
			}
			cols, err := rows.Columns()
			if err != nil || len(cols) != 3 {
				t.Fatalf("columns %v %v", cols, err)
			}
			if rows.Next() || rows.Err() != nil {
				t.Fatal("expected empty result")
			}
			rows.Close()
			exec("DROP TABLE items", 0)
		}()
	}
}
func TestPacketFragmentation(t *testing.T) {
	for _, n := range []int{0, 251, maxPacket, maxPacket + 1} {
		a, b := net.Pipe()
		a.SetDeadline(time.Now().Add(5 * time.Second))
		b.SetDeadline(time.Now().Add(5 * time.Second))
		payload := bytes.Repeat([]byte{'x'}, n)
		done := make(chan error, 1)
		go func() { defer a.Close(); done <- (&packets{conn: a}).write(payload) }()
		got, err := (&packets{conn: b}).read()
		b.Close()
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("size %d: %v", n, err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
func TestBadSequence(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	go func() { defer a.Close(); io.Copy(a, bytes.NewReader([]byte{0, 0, 0, 3})) }()
	if _, err := (&packets{conn: b}).read(); err == nil {
		t.Fatal("accepted bad sequence")
	}
}
