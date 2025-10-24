package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"

	"github.com/xtaci/tcpraw/faketcp"
)

var (
	addr = flag.String("l", "", "listen address")
)

func main() {
	flag.Parse()
	if addr == nil || *addr == "" {
		fmt.Printf("addr unset, eg ./%s -l 0.0.0.0:9191 \n", os.Args[0])
		return
	}
	ctx := context.Background()
	l := faketcp.NewFakeTcpListener(ctx)
	if err := l.Listen(*addr); err != nil {
		panic(err)
	}
	log.Println("server listening on:", *addr)
	for {
		conn, err := l.Accept()
		if err != nil {
			panic(err)
		}

		log.Println("ok, server accepted client:", conn.RemoteAddr(), "local addr:", conn.LocalAddr())
		go handleClient(conn)
	}
}

func handleClient(conn net.Conn) {
	defer conn.Close()
	for {
		buf := make([]byte, 1024)
		n, err := conn.Read(buf)
		if err != nil {
			panic(err)
		}
		log.Printf("server received from fakeconn: %s, bytes: %d, data:%s", conn.RemoteAddr().String(), n, string(buf[:n]))
		n, err = conn.Write(buf[:n])
		if err != nil {
			panic(err)
		}
		log.Println("ok server send bytes:", n, "data:", string(buf[:n]))
	}
}
