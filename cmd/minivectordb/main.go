package main

import (
	"flag"
	"log"
	"minivectordb/internal/db"
	"minivectordb/internal/mysqlwire"
	"net"
)

func main() {
	addr := flag.String("addr", ":3306", "MySQL TCP listen address")
	data := flag.String("data", "./data", "snapshot directory")
	flag.Parse()
	e, err := db.Open(*data)
	if err != nil {
		log.Fatal(err)
	}
	l, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()
	log.Printf("MiniVectorDB M2 MySQL listening on %s", l.Addr())
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go mysqlwire.Serve(c, e)
	}
}
