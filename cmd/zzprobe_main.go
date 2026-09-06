package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/grandcat/zeroconf"
)

func main() {
	c, err := net.DialTimeout("tcp", "192.168.68.114:8080", 3*time.Second)
	fmt.Println("tcp dial:", c != nil, err)
	r, err := zeroconf.NewResolver(nil)
	fmt.Println("resolver:", err)
	ch := make(chan *zeroconf.ServiceEntry)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go func() {
		for e := range ch {
			fmt.Println("entry:", e.Instance, e.HostName, e.AddrIPv4, e.Port, e.Text)
		}
	}()
	fmt.Println("browse:", r.Browse(ctx, "_scanner._tcp", "local.", ch))
	<-ctx.Done()
}
