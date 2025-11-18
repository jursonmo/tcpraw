package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/jursonmo/tcpraw"
	"github.com/jursonmo/tcpraw/faketcp"
)

var (
	addr      = flag.String("addr", "", "server address")
	count     = flag.Int("c", 3, "send count")
	batchSize = flag.Int("b", 0, "batch size")

	fecDataShards   = flag.Int("d", 10, "fec data shards")
	fecParityShards = flag.Int("p", 2, "fec parity shards")
)

func main() {
	flag.Parse()
	if addr == nil || *addr == "" {
		fmt.Printf("addr unset, eg ./%s -addr 2.2.2.2:9191 \n", os.Args[0])
		return
	}
	if batchSize != nil && *batchSize > 0 {
		tcpraw.BatchSize = *batchSize
	}

	opts := []faketcp.ClientOption{}
	if fecDataShards != nil && *fecDataShards > 0 && fecParityShards != nil && *fecParityShards > 0 {
		log.Printf("fec enabled, data shards: %d, parity shards: %d", *fecDataShards, *fecParityShards)
		if fecDataShards != nil && *fecDataShards > 0 && fecParityShards != nil && *fecParityShards > 0 {
			opts = append(opts, faketcp.WithClientFec(*fecDataShards, *fecParityShards))
		}
	}

	ctx := context.Background()
	conn, err := faketcp.Dial(ctx, *addr, opts...)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	log.Println("ok, client connected to server:", conn.RemoteAddr().String(), "local addr:", conn.LocalAddr().String())
	go func() {
		for {
			buf := make([]byte, 1024)
			n, err := conn.Read(buf)
			if err != nil {
				panic(err)
			}
			log.Printf("client received from fakeconn: %s, bytes: %d, data:%s", conn.RemoteAddr().String(), n, string(buf[:n]))
		}
	}()

	for i := 0; i < *count; i++ {
		if i%2 == 1 {
			b := []byte(fmt.Sprintf("hello %d", i))
			sendData(conn, b)
			continue
		}
		time.Sleep(time.Second)
		bb := []byte(fmt.Sprintf("xx hello server %d", i))
		sendData(conn, bb)
	}
}

func sendData(conn net.Conn, data []byte) {
	n, err := conn.Write(data)
	if err != nil {
		panic(err)
	}
	log.Println("ok client send:", string(data), "bytes:", n)
}
