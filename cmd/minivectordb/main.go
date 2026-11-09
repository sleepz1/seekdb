package main

import (
	"flag"
	"log"
	"minivectordb/internal/db"
	"minivectordb/internal/mysqlwire"
	"net"
	"os"
)

func main() {
	var addr string
	flag.StringVar(&addr, "addr", "", "optional MySQL helper listen address (replaces stdin mode)")
	flag.StringVar(&addr, "mysql-addr", "", "optional MySQL helper listen address (replaces stdin mode)")
	data := flag.String("data", "/data", "WAL and optional snapshot directory")
	flag.Bool("ann", false, "reserved no-op; searches remain exact")
	flag.Parse()
	e, err := db.Open(*data)
	if err != nil {
		log.Fatal(err)
	}
	if addr == "" {
		if err := repl(os.Stdin, os.Stdout, e.Session()); err != nil {
			log.Fatal(err)
		}
		return
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()
	log.Printf("MiniVectorDB optional MySQL listening on %s", l.Addr())
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go mysqlwire.Serve(c, e)
	}
}
