package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/xtaci/tcpraw/faketcp"
)

var (
	addr  = flag.String("addr", "", "server address")
	count = flag.Int("c", 3, "send count")
)

func main() {
	flag.Parse()
	if addr == nil || *addr == "" {
		fmt.Printf("addr unset, eg ./%s -addr 2.2.2.2:9191 \n", os.Args[0])
		return
	}
	//ctx := context.Background()
	conn, err := faketcp.Dial("", *addr)
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
		time.Sleep(time.Second)
		b := []byte(fmt.Sprintf("hello server %d", i))
		n, err := conn.Write(b)
		if err != nil {
			panic(err)
		}
		log.Println("ok client send:", string(b), "bytes:", n)
	}
}
