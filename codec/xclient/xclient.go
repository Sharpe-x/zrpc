package xclient

import (
	"context"
	"io"
	"reflect"
	"sync"
	"zrpc"
)

type XClient struct {
	d       Discovery
	mode    SelectMode
	opt     *zrpc.Option
	mu      sync.Mutex
	clients map[string]*zrpc.Client
}

var _ io.Closer = (*XClient)(nil)

// NewXClient 的构造函数需要传入三个参数，服务发现实例 Discovery、负载均衡模式 SelectMode 以及协议选项 Option。
//为了尽量地复用已经创建好的 Socket 连接，使用 clients 保存创建成功的 Client 实例，并提供 Close 方法在结束后，关闭已经建立的连接。
func NewXClient(d Discovery, mode SelectMode, opt *zrpc.Option) *XClient {
	return &XClient{
		d:       d,
		mode:    mode,
		clients: make(map[string]*zrpc.Client),
	}
}

func (xc *XClient) Close() error {
	xc.mu.Lock()
	defer xc.mu.Unlock()
	for key, client := range xc.clients {
		_ = client.Close()
		delete(xc.clients, key)
	}
	return nil
}

// 复用 Client 的能力封装在方法 dial 中
//  检查 xc.clients 是否有缓存的 Client，如果有，检查是否是可用状态，如果是则返回缓存的 Client，如果不可用，则从缓存中删除。
// 如果步骤 1) 没有返回缓存的 Client，则说明需要创建新的 Client，缓存并返回。
func (xc *XClient) dial(rpcAddr string) (*zrpc.Client, error) {
	xc.mu.Lock()
	defer xc.mu.Unlock()
	client, ok := xc.clients[rpcAddr]
	if ok && !client.IsAvailable() {
		_ = client.Close()
		delete(xc.clients, rpcAddr)
		client = nil
	}

	if client == nil {
		var err error
		client, err = zrpc.XDial(rpcAddr, xc.opt)
		if err != nil {
			return nil, err
		}
		xc.clients[rpcAddr] = client
	}
	return client, nil
}

func (xc *XClient) call(rpcAddr string, ctx context.Context, serviceMethod string, args, reply interface{}) error {
	client, err := xc.dial(rpcAddr)
	if err != nil {
		return err
	}
	return client.Call(ctx, serviceMethod, args, reply)
}

func (xc *XClient) Call(ctx context.Context, serviceMethod string, args, reply interface{}) error {
	rpcAddr, err := xc.d.Get(xc.mode)
	if err != nil {
		return err
	}
	return xc.call(rpcAddr, ctx, serviceMethod, args, reply)
}

// Broadcast 将请求广播到所有的服务实例，如果任意一个实例发生错误，则返回其中一个错误；如果调用成功，则返回其中一个的结果。有以下几点需要注意：
// 为了提升性能，请求是并发的
// 并发情况下需要使用互斥锁保证 error 和 reply 能被正确赋值。
// 借助 context.WithCancel 确保有错误发生时，快速失败。
func (xc *XClient) Broadcast(ctx context.Context, serviceMethod string, args, reply interface{}) error {
	servers, err := xc.d.GetAll()
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var e error
	replyDone := reply == nil

	ctx, canCel := context.WithCancel(ctx)

	for _, server := range servers {
		wg.Add(1)
		go func(rpcAddr string) {
			defer wg.Done()
			var cloneReply interface{}
			if reply != nil {
				cloneReply = reflect.New(reflect.ValueOf(reply).Elem().Type()).Interface()
			}
			callErr := xc.call(rpcAddr, ctx, serviceMethod, args, cloneReply)
			mu.Lock()
			if callErr != nil && e == nil {
				e = callErr
				canCel()
			}
			if callErr == nil && !replyDone {
				reflect.ValueOf(reply).Elem().Set(reflect.ValueOf(cloneReply).Elem())
				replyDone = true
			}
			mu.Unlock()
		}(server)
	}
	wg.Wait()
	canCel()
	return e
}
