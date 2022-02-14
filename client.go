package zrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"zrpc/codec"
)

// 对 net/rpc 而言，一个函数需要能够被远程调用，需要满足如下五个条件：
// the method’s type is exported.
// the method is exported.
// the method has two arguments, both exported (or builtin) types.
// the method’s second argument is a pointer.
// the method has return type error.
// example: func (t *T) MethodName(argType T1, replyType *T2) error

type Call struct {
	Seq           uint64
	ServiceMethod string      //  format "<service>.<method>"
	Args          interface{} //  arguments to the function
	Reply         interface{} //  reply from the function
	Error         error       //  if error occurs, it will be set
	Done          chan *Call  //  Strobes when call is complete.
}

func (c *Call) done() {
	c.Done <- c
}

type Client struct {
	cc      codec.Codec //cc 是消息的编解码器，和服务端类似，用来序列化将要发送出去的请求，以及反序列化接收到的响应。
	opt     *Option
	sending sync.Mutex       // sending 是一个互斥锁，和服务端类似，为了保证请求的有序发送，即防止出现多个请求报文混淆。
	header  codec.Header     // header 是每个请求的消息头，header 只有在请求发送时才需要，而请求发送是互斥的，因此每个客户端只需要一个，声明在 Client 结构体中可以复用。
	mu      sync.Mutex       // protect following
	seq     uint64           //用于给发送的请求编号，每个请求拥有唯一编号。
	pending map[uint64]*Call //  存储未处理完的请求，键是编号，值是 Call 实例。
	// 任意一个值置为 true，则表示 Client 处于不可用的状态，但有些许的差别，closing 是用户主动关闭的，即调用 Close 方法，而 shutdown 置为 true 一般是有错误发生。
	closing  bool
	shutdown bool
}

var _ io.Closer = (*Client)(nil)
var ErrShutdown = errors.New("connection is shut down")

// Close 关闭连接
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closing {
		return ErrShutdown
	}
	c.closing = true
	return c.cc.Close()
}

func (c *Client) IsAvailable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.shutdown && !c.closing
}

// registerCall：将参数 call 添加到 client.pending 中，并更新 client.seq。
func (c *Client) registerCall(call *Call) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closing || c.shutdown {
		return 0, ErrShutdown
	}

	call.Seq = c.seq
	c.pending[call.Seq] = call
	c.seq++

	return call.Seq, nil
}

// removeCall：根据 seq，从 client.pending 中移除对应的 call，并返回。
func (c *Client) removeCall(seq uint64) *Call {
	c.mu.Lock()
	defer c.mu.Unlock()

	call := c.pending[seq]
	delete(c.pending, seq)
	return call
}

// terminateCalls：服务端或客户端发生错误时调用，将 shutdown 设置为 true，且将错误信息通知所有 pending 状态的 call。
func (c *Client) terminateCalls(err error) {
	c.sending.Lock()
	defer c.sending.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()

	c.shutdown = true
	for _, call := range c.pending {
		call.Error = err
		call.done()
	}
}

// 对一个客户端端来说，接收响应、发送请求是最重要的 2 个功能。那么首先实现接收功能，接收到的响应有三种情况：
// call 不存在，可能是请求没有发送完整，或者因为其他原因被取消，但是服务端仍旧处理了。
// call 存在，但服务端处理出错，即 h.Error 不为空。
// call 存在，服务端处理正常，那么需要从 body 中读取 Reply 的值。

func (c *Client) receive() {
	var err error
	for err == nil {
		var h codec.Header
		if err = c.cc.ReadHeader(&h); err != nil {
			break
		}
		call := c.removeCall(h.Seq)
		switch {
		case call == nil:
			// it usually means that Write partially failed
			// and call was already removed.
			err = c.cc.ReadBody(nil)
		case h.Error != "":
			call.Error = fmt.Errorf(h.Error)
			err = c.cc.ReadBody(nil)
			call.done()
		default:
			err = c.cc.ReadBody(call.Reply)
			if err != nil {
				call.Error = errors.New("reading body " + err.Error())
			}
			call.done()
		}
	}
	c.terminateCalls(err)
}

type clientResult struct {
	client *Client
	err    error
}

type newClientFunc func(conn net.Conn, opt *Option) (client *Client, err error)

func dialTimeout(f newClientFunc, network, address string, opts ...*Option) (client *Client, err error) {
	opt, err := parseOptions(opts...)
	if err != nil {
		return nil, err
	}

	conn, err := net.DialTimeout(network, address, opt.ConnectTimeout)
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()

	ch := make(chan clientResult)
	go func() {
		client, err = f(conn, opt)
		ch <- clientResult{
			client: client,
			err:    err,
		}
	}()

	if opt.ConnectTimeout == 0 {
		result := <-ch
		return result.client, result.err
	}

	select {
	case <-time.After(opt.ConnectTimeout):
		return nil, fmt.Errorf("rpc client: connect timeout: expect within %s", opt.ConnectTimeout)
	case result := <-ch:
		return result.client, result.err
	}

}

// NewClient 实例时，首先需要完成一开始的协议交换，即发送 Option 信息给服务端。协商好消息的编解码方式之后，再创建一个子协程调用 receive() 接收响应。
func NewClient(conn net.Conn, opt *Option) (*Client, error) {
	f := codec.NewCodeFuncMap[opt.CodecType]
	if f == nil {
		err := fmt.Errorf("invalid codec type: %v", opt.CodecType)
		log.Println("rpc client: codec error:", err)
		return nil, err
	}

	// send options with server
	if err := json.NewEncoder(conn).Encode(opt); err != nil {
		log.Println("rpc client: options error:", err)
		_ = conn.Close()
		return nil, err
	}

	return newClientCodec(f(conn), opt), nil

}

func newClientCodec(cc codec.Codec, opt *Option) *Client {
	client := &Client{
		seq:     1,
		cc:      cc,
		opt:     opt,
		pending: make(map[uint64]*Call),
	}
	go client.receive()
	return client
}

func parseOptions(opts ...*Option) (*Option, error) {
	// if opts id nil or pass nil as parameter
	if len(opts) == 0 || opts[0] == nil {
		return DefaultOption, nil
	}

	if len(opts) != 1 {
		return nil, errors.New("number of options is more than 1")
	}

	opt := opts[0]
	opt.MagicNumber = DefaultOption.MagicNumber
	if opt.CodecType == "" {
		opt.CodecType = DefaultOption.CodecType
	}
	if opt.ConnectTimeout == 0 {
		opt.ConnectTimeout = DefaultOption.ConnectTimeout
	}

	return opt, nil
}

//Dial 函数，便于用户传入服务端地址，创建 Client 实例。为了简化用户调用，通过 ...*Option 将 Option 实现为可选参数。
func Dial(network, address string, opts ...*Option) (client *Client, err error) {
	/*opt, err := parseOptions(opts...)
	if err != nil {
		return nil, err
	}
	conn, err := net.Dial(network, address)
	if err != nil {
		return nil, err
	}

	defer func() {
		if client == nil {
			_ = conn.Close()
		}
	}()

	return NewClient(conn, opt)*/
	return dialTimeout(NewClient, network, address, opts...)
}

func (c *Client) send(call *Call) {
	// make sure that the client will send a complete request
	c.sending.Lock()
	defer c.sending.Unlock()

	seq, err := c.registerCall(call)
	if err != nil {
		call.Error = err
		call.done()
		return
	}

	// prepare request header
	c.header.ServiceMethod = call.ServiceMethod
	c.header.Seq = seq
	c.header.Error = ""

	// encode and send request
	if err = c.cc.Write(&c.header, call.Args); err != nil {
		call = c.removeCall(seq)
		// call may be nil, it usually means that Write partially failed,
		// client has received the response and handled
		if call != nil {
			call.Error = err
			call.done()
		}
	}

}

// Go 和 Call 是客户端暴露给用户的两个 RPC 服务调用接口，Go 是一个异步接口，返回 call 实例。
func (c *Client) Go(serviceMethod string, args, reply interface{}, done chan *Call) *Call {
	if done == nil {
		done = make(chan *Call, 10)
	} else if cap(done) == 0 {
		log.Panic("rpc client: done channel is unbuffered")
	}

	call := &Call{
		ServiceMethod: serviceMethod,
		Args:          args,
		Reply:         reply,
		Done:          done,
	}
	c.send(call)
	return call
}

// Call invokes the named function, waits for it to complete,
// and returns its error status.
func (c *Client) Call(ctx context.Context, serviceMethod string, args, reply interface{}) error {
	call := c.Go(serviceMethod, args, reply, make(chan *Call, 1))
	select {
	case <-ctx.Done():
		c.removeCall(call.Seq)
		return errors.New("rpc client: call failed: " + ctx.Err().Error())
	case ca := <-call.Done:
		return ca.Error
	}
}

func NewHttpClient(conn net.Conn, opt *Option) (*Client, error) {
	_, _ = io.WriteString(conn, fmt.Sprintf("CONNECT %s HTTP/1.0\n\n", defaultRPCPath))
	// Require successful HTTP response
	// before switching to RPC protocol.

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{
		Method: "CONNECT",
	})

	if err == nil && resp.Status == connected {
		return NewClient(conn, opt)
	}
	if err == nil {
		err = errors.New("unexpected HTTP response: " + resp.Status)
	}
	return nil, err
}

func DialHTTP(network, address string, opts ...*Option) (*Client, error) {
	return dialTimeout(NewHttpClient, network, address, opts...)
}

// 通过 HTTP CONNECT 请求建立连接之后，后续的通信过程就交给 NewClient 了。
// 为了简化调用，提供了一个统一入口 XDial

func XDial(rpcAddr string, opts ...*Option) (*Client, error) {
	parts := strings.Split(rpcAddr, "@")
	if len(parts) != 2 {
		return nil, fmt.Errorf("rpc client err: wrong format '%s', expect protocol@addr", rpcAddr)
	}
	protocol, addr := parts[0], parts[1]
	switch protocol {
	case "http":
		return DialHTTP("tcp", addr, opts...)
	default:
		// tcp, unix or other transport protocol
		return Dial(protocol, addr, opts...)
	}
}
