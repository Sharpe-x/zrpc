package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
	"zrpc"
	"zrpc/codec/registry"
	"zrpc/codec/xclient"
)

type FooDiscoveryTwo int

type ArgsDiscoveryTwo struct {
	Num1, Num2 int
}

func (f FooDiscoveryTwo) Sum(args ArgsDiscoveryTwo, reply *int) error {
	*reply = args.Num1 + args.Num2
	return nil
}

func (f FooDiscoveryTwo) Sleep(args ArgsDiscoveryTwo, reply *int) error {
	time.Sleep(time.Second * time.Duration(args.Num1))
	*reply = args.Num1 + args.Num2
	return nil
}

func startRegistry(wg *sync.WaitGroup) {
	l, _ := net.Listen("tcp", ":9999")
	registry.HandleHTTP()
	wg.Done()
	_ = http.Serve(l, nil)
}

func startServerByZrpcDiscovery(registryAddr string, wg *sync.WaitGroup) {
	var fooDiscovery FooDiscoveryTwo
	l, _ := net.Listen("tcp", ":0")
	server := zrpc.NewServer()
	_ = server.Register(&fooDiscovery)
	registry.Heartbeat(registryAddr, "tcp@"+l.Addr().String(), 0)
	wg.Done()
	server.Accept(l)
}

func call2(registry string) {
	d := xclient.NewZrpcRegistryDiscovery(registry, 0)
	xc := xclient.NewXClient(d, xclient.RandomSelect, nil)
	defer func() { _ = xc.Close() }()
	// send request & receive response
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			foo2(xc, context.Background(), "call", "FooDiscoveryTwo.Sum", &ArgsDiscoveryTwo{Num1: i, Num2: i * i})
		}(i)
	}
	wg.Wait()
}

func foo2(xc *xclient.XClient, ctx context.Context, typ, serviceMethod string, args *ArgsDiscoveryTwo) {
	var reply int
	var err error
	switch typ {
	case "call":
		err = xc.Call(ctx, serviceMethod, args, &reply)
	case "broadcast":
		err = xc.Broadcast(ctx, serviceMethod, args, &reply)
	}
	if err != nil {
		log.Printf("%s %s error: %v", typ, serviceMethod, err)
	} else {
		log.Printf("%s %s success: %d + %d = %d", typ, serviceMethod, args.Num1, args.Num2, reply)
	}
}

func broadcast2(registry string) {
	d := xclient.NewZrpcRegistryDiscovery(registry, 0)
	xc := xclient.NewXClient(d, xclient.RandomSelect, nil)
	defer func() { _ = xc.Close() }()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			foo2(xc, context.Background(), "broadcast", "FooDiscoveryTwo.Sum", &ArgsDiscoveryTwo{Num1: i, Num2: i * i})
			// expect 2 - 5 timeout
			ctx, _ := context.WithTimeout(context.Background(), time.Second*2)
			foo2(xc, ctx, "broadcast", "FooDiscoveryTwo.Sleep", &ArgsDiscoveryTwo{Num1: i, Num2: i * i})
		}(i)
	}
	wg.Wait()
}

func main() {
	log.SetFlags(0)
	registryAddr := "http://127.0.0.1:9999/_zrpc_/registry"
	var wg sync.WaitGroup
	wg.Add(1)
	go startRegistry(&wg)
	wg.Wait()

	time.Sleep(time.Second)
	wg.Add(2)
	go startServerByZrpcDiscovery(registryAddr, &wg)
	go startServerByZrpcDiscovery(registryAddr, &wg)
	wg.Wait()

	time.Sleep(time.Second)
	call2(registryAddr)
	broadcast2(registryAddr)
}
