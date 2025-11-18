package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"

	"github.com/jursonmo/tcpraw"
	"github.com/jursonmo/tcpraw/faketcp"
)

var (
	addr      = flag.String("l", "", "listen address")
	batchSize = flag.Int("b", 0, "batch size")

	// fec options
	fecDataShards   = flag.Int("d", 10, "fec data shards")
	fecParityShards = flag.Int("p", 2, "fec parity shards")
)

func main() {
	flag.Parse()
	if addr == nil || *addr == "" {
		fmt.Printf("addr unset, eg ./%s -l x.x.x.x:9191,y.y.y.y:9191 \n", os.Args[0])
		return
	}
	if batchSize != nil && *batchSize > 0 {
		tcpraw.BatchSize = *batchSize
	}

	opts := []faketcp.ListenerOption{}
	if fecDataShards != nil && *fecDataShards > 0 && fecParityShards != nil && *fecParityShards > 0 {
		log.Printf("fec enabled, data shards: %d, parity shards: %d", *fecDataShards, *fecParityShards)
		if fecDataShards != nil && *fecDataShards > 0 && fecParityShards != nil && *fecParityShards > 0 {
			opts = append(opts, faketcp.WithListenerFec(*fecDataShards, *fecParityShards))
		}
	}

	ctx := context.Background()
	// l := faketcp.NewFakeTcpListener(ctx)
	// if err := l.Listen(*addr); err != nil {
	// 	panic(err)
	// }
	// 侦听单个地址
	//l, err := faketcp.FakeTcpListen(ctx, *addr)

	//可以单个或多个多个地址
	l, err := faketcp.FakeTcpListeners(ctx, strings.Split(*addr, ","), opts...)
	if err != nil {
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
