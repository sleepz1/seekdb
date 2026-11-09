package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"minivectordb/internal/db"
	"net"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", ":3306", "TCP listen or client address")
	data := flag.String("data", "./data", "snapshot directory")
	query := flag.String("query", "", "execute one SQL statement as a client")
	flag.Parse()
	if *query != "" {
		c, err := net.DialTimeout("tcp", *addr, 3*time.Second)
		if err != nil {
			log.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err = fmt.Fprintln(c, *query); err != nil {
			log.Fatal(err)
		}
		var r db.Result
		if err = json.NewDecoder(c).Decode(&r); err != nil {
			log.Fatal(err)
		}
		json.NewEncoder(os.Stdout).Encode(r)
		if r.Error != "" {
			os.Exit(1)
		}
		return
	}
	e, err := db.Open(*data)
	if err != nil {
		log.Fatal(err)
	}
	l, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()
	log.Printf("MiniVectorDB M1 line SQL listening on %s", l.Addr())
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go serve(c, e)
	}
}
func serve(c net.Conn, e *db.Engine) {
	defer c.Close()
	s := e.Session()
	scan := bufio.NewScanner(c)
	scan.Buffer(make([]byte, 4096), 1024*1024)
	enc := json.NewEncoder(c)
	for scan.Scan() {
		if err := enc.Encode(s.Execute(scan.Text())); err != nil {
			return
		}
	}
}
