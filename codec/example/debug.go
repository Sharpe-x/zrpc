package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
	"zrpc"
)

type FooInt int
type ArgsInt struct {
	Num1, Num2 int
}

func (f FooInt) Sum(args ArgsInt, reply *int) error {
	*reply = args.Num1 + args.Num2
	return nil
}

func startDebugServer(addrCh chan string) {
	var foo FooInt
	l, _ := net.Listen("tcp", ":0")
	_ = zrpc.Register(&foo)
	zrpc.HandleHTTP()
	addrCh <- l.Addr().String()
	fmt.Println("http server started:", l.Addr().String())
	_ = http.Serve(l, nil)
}

func call(addrCh chan string) {
	client, _ := zrpc.DialHTTP("tcp", <-addrCh)
	defer func() { _ = client.Close() }()

	time.Sleep(time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(i)
		go func(i int) {
			defer wg.Done()
			args := &ArgsInt{Num1: i, Num2: i * i}
			var reply int
			if err := client.Call(context.Background(), "FooInt.Sum", args, &reply); err != nil {
				log.Fatal("call FooInt.Sum error:", err)
			}
			log.Printf("%d + %d = %d", args.Num1, args.Num2, reply)
		}(i)
	}
	wg.Wait()
}

func main() {
	log.SetFlags(0)
	ch := make(chan string)
	go call(ch)
	startDebugServer(ch)
}
