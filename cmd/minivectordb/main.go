package main

import (
	"flag"
	"fmt"
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
	ann := flag.Bool("ann", false, "approximate search using every second filtered candidate")
	budget := flag.Int64("memory-budget", 64<<20, "decoded row cache budget in bytes (0 unlimited)")
	flag.Parse()
	e, err := db.OpenWithOptions(*data, db.OpenOptions{MemoryBudgetBytes: *budget})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintln(os.Stderr, "minivectordb: ready")
	e.SetDefaultANN(*ann)
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
