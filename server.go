package zrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"
	"zrpc/codec"
)

/*
客户端与服务端的通信需要协商一些内容，例如 HTTP 报文，分为 header 和 body 2 部分，body 的格式和长度通过 header 中的 Content-Type 和 Content-Length 指定，
服务端通过解析 header 就能够知道如何从 body 中读取需要的信息。对于 RPC 协议来说，这部分协商是需要自主设计的。
为了提升性能，一般在报文的最开始会规划固定的字节，来协商相关的信息。比如第1个字节用来表示序列化方式，第2个字节表示压缩方式，第3-6字节表示 header 的长度，7-10 字节表示 body 的长度。

对于 zrpc 来说，目前需要协商的唯一一项内容是消息的编解码方式。我们将这部分信息，放到结构体 Option 中承载。
*/

const MagicNumber = 0x3bef5c

type Option struct {
	MagicNumber int        // MagicNumber marks  a zrpc request
	CodecType   codec.Type // client may choose different Codec to encode body
	// 超时处理是 RPC 框架一个比较基本的能力，如果缺少超时处理机制，无论是服务端还是客户端都容易因为网络或其他错误导致挂死，资源耗尽，这些问题的出现大大地降低了服务的可用性。因此，我们需要在 RPC 框架中加入超时处理的能力。
	// 需要客户端处理超时的地方有 与服务端建立连接，导致的超时 发送请求到服务端，写报文导致的超时 等待服务端处理时，等待处理导致的超时（比如服务端已挂死，迟迟不响应） 从服务端接收响应时，读报文导致的超时
	// 需要服务端处理超时的地方有 读取客户端请求报文时，读报文导致的超时 发送响应报文时，写报文导致的超时 调用映射服务的方法时，处理报文导致的超时
	ConnectTimeout time.Duration //
	HandleTimeout  time.Duration //
}

var DefaultOption = &Option{
	MagicNumber:    MagicNumber,
	CodecType:      codec.GobType,
	ConnectTimeout: time.Second * 10,
}

/*
一般来说，涉及协议协商的这部分信息，需要设计固定的字节来传输的。
但是为了实现上更简单，zrpc 客户端固定采用 JSON 编码 Option，
后续的 header 和 body 的编码方式由 Option 中的 CodeType 指定，
服务端首先使用 JSON 解码 Option，然后通过 Option的 CodeType 解码剩余的内容。即报文将以这样的形式发送：

| Option{MagicNumber: xxx, CodecType: xxx} | Header{ServiceMethod ...} | Body interface{} |
| <------      固定 JSON 编码      ------>  | <-------   编码方式由 CodeType 决定   ------->|

在一次连接中，Option 固定在报文的最开始，Header 和 Body 可以有多个，即报文可能是这样的。
*/

// Server represents a rpc server
type Server struct {
	serviceMap sync.Map
}

func (s *Server) Register(rcvr interface{}) error {
	serv := newService(rcvr)
	if _, dup := s.serviceMap.LoadOrStore(serv.name, serv); dup {
		return errors.New("rpc: service already defined: " + serv.name)
	}
	return nil
}

func Register(revr interface{}) error {
	return DefaultServer.Register(revr)
}

func (s *Server) findService(serviceMethod string) (srv *service, mtype *methodType, err error) {
	dot := strings.LastIndex(serviceMethod, ".")
	if dot < 0 {
		err = errors.New("rpc server: service/method request ill-formed: " + serviceMethod)
		return
	}

	serviceName, methodName := serviceMethod[:dot], serviceMethod[dot+1:]
	serv, ok := s.serviceMap.Load(serviceName)
	if !ok {
		err = errors.New("rpc server: can't find service " + serviceName)
		return
	}

	srv = serv.(*service)
	mtype = srv.method[methodName]
	if mtype == nil {
		err = errors.New("rpc server: can't find method " + methodName)
	}
	return
}

// NewServer creates a new rpc server
func NewServer() *Server {
	return &Server{}
}

// DefaultServer is the default instance of *Server
var DefaultServer = NewServer()

func (s *Server) Accept(lis net.Listener) { // net.Listener 作为参数，for 循环等待 socket 连接建立，并开启子协程处理，处理过程交给了 ServerConn 方法。
	for {
		conn, err := lis.Accept()
		if err != nil {
			log.Println("rpc server: accept error:", err)
			return
		}

		go s.ServeConn(conn)
	}
}

// Accept accepts connections on the listener and serves requests
// for each incoming connection.
func Accept(lis net.Listener) {
	DefaultServer.Accept(lis) // DefaultServer 是一个默认的 Server 实例，主要为了用户使用方便。

}

// 启动服务，传入 listener 即可，tcp 协议和 unix 协议都支持。
//lis,_ := net.Listen("tcp",":9999") zrpc.Accept(lis)

// ServeConn runs the server on a single connection.
// ServeConn blocks, serving the connection until the client hangs up.
func (s *Server) ServeConn(conn io.ReadWriteCloser) {

	defer func() {
		_ = conn.Close()
	}()

	// 首先使用 json.NewDecoder 反序列化得到 Option 实例，检查 MagicNumber 和 CodeType 的值是否正确。
	var opt Option
	if err := json.NewDecoder(conn).Decode(&opt); err != nil {
		log.Println("rpc server: option error: ", err)
		return
	}

	if opt.MagicNumber != MagicNumber {
		log.Printf("rpc server: invalid magic number %x", opt.MagicNumber)
		return
	}

	f := codec.NewCodeFuncMap[opt.CodecType]
	if f == nil {
		log.Printf("rpc server: invalid codec type %s", opt.CodecType)
		return
	}

	//然后根据 CodeType 得到对应的消息编解码器，接下来的处理交给 serverCodec。
	s.serveCodec(f(conn), &opt)
}

func (s *Server) serveCodec(cc codec.Codec, opt *Option) {
	sending := new(sync.Mutex)
	wg := new(sync.WaitGroup)

	for { // 在一次连接中，允许接收多个请求，即多个 request header 和 request body，因此这里使用了 for 无限制地等待请求的到来
		req, err := s.readRequest(cc) // 读取请求 readRequest
		if err != nil {
			if req == nil {
				break // it's not possible to recover, so close the connection
			}
			// 尽力而为，只有在 header 解析失败时，才终止循环。
			req.h.Error = err.Error()
			s.sendResponse(cc, req.h, struct{}{}, sending)
			return
		}
		wg.Add(1)
		// 处理请求是并发的，但是回复请求的报文必须是逐个发送的，并发容易导致多个回复报文交织在一起，
		// 客户端无法解析。在这里使用锁(sending)保证。
		go s.handleRequest(cc, req, sending, wg, opt.HandleTimeout) // 处理请求 handleRequest
	}

	wg.Wait()
	_ = cc.Close()
}

func (s *Server) readRequestHeader(cc codec.Codec) (*codec.Header, error) {
	var h codec.Header
	if err := cc.ReadHeader(&h); err != nil {
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			log.Println("rpc error: read header error: ", err)
		}
		return nil, err
	}

	return &h, nil

}

func (s *Server) readRequest(cc codec.Codec) (*request, error) {
	h, err := s.readRequestHeader(cc)
	if err != nil {
		return nil, err
	}

	req := &request{
		h: h,
	}

	req.svc, req.mtype, err = s.findService(h.ServiceMethod)
	if err != nil {
		return req, err
	}

	req.argv = req.mtype.newArgv()
	req.replyv = req.mtype.newReplyv()

	argvi := req.argv.Interface()
	if req.argv.Type().Kind() != reflect.Ptr {
		argvi = req.argv.Addr().Interface()
	}

	if err = cc.ReadBody(argvi); err != nil {
		log.Println("rpc server: read argv err: ", err)
	}
	return req, nil
}

func (s *Server) handleRequest(cc codec.Codec, req *request, sending *sync.Mutex, wg *sync.WaitGroup, timeout time.Duration) {
	defer wg.Done()
	called := make(chan struct{})
	sent := make(chan struct{})
	go func() {
		err := req.svc.call(req.mtype, req.argv, req.replyv)
		called <- struct{}{}

		if err != nil {
			req.h.Error = err.Error()
			s.sendResponse(cc, req.h, "invalidRequest", sending)
			sent <- struct{}{}
			return
		}

		s.sendResponse(cc, req.h, req.replyv.Interface(), sending)
		sent <- struct{}{}
	}()

	if timeout == 0 {
		<-called
		<-sent
		return
	}

	select {
	case <-time.After(timeout):
		req.h.Error = fmt.Sprintf("rpc server: request handle timeout: expect within %s", timeout)
		s.sendResponse(cc, req.h, "invalidRequest", sending)
	case <-called:
		<-sent
	}
}

func (s *Server) sendResponse(cc codec.Codec, h *codec.Header, body interface{}, sending *sync.Mutex) {
	sending.Lock()
	defer sending.Unlock()

	if err := cc.Write(h, body); err != nil {
		log.Println("rpc server: write response error:", err)
	}
}

type request struct {
	h            *codec.Header
	argv, replyv reflect.Value
	mtype        *methodType
	svc          *service
}

// Web 开发中，我们经常使用 HTTP 协议中的 HEAD、GET、POST 等方式发送请求，等待响应。
// 但 RPC 的消息格式与标准的 HTTP 协议并不兼容，在这种情况下，就需要一个协议的转换过程。
// HTTP 协议的 CONNECT 方法恰好提供了这个能力，CONNECT 一般用于代理服务。
// 假设浏览器与服务器之间的 HTTPS 通信都是加密的，浏览器通过代理服务器发起 HTTPS 请求时，
// 由于请求的站点地址和端口号都是加密保存在 HTTPS 请求报文头中的，代理服务器如何知道往哪里发送请求呢？
//为了解决这个问题，浏览器通过 HTTP 明文形式向代理服务器发送一个 CONNECT
//请求告诉代理服务器目标地址和端口，代理服务器接收到这个请求后，会在对应端口与目标站点建立一个 TCP 连接，
//连接建立成功后返回 HTTP 200 状态码告诉浏览器与该站点的加密通道已经完成。接下来代理服务器仅需透传浏览器和服务器之间的加密数据包即可，代理服务器无需解析 HTTPS 报文。
// example
//  浏览器向代理服务器发送 CONNECT 请求。 CONNECT test.com:443 HTTP/1.0
// 代理服务器返回 HTTP 200 状态码表示连接已经建立。 HTTP/1.0 200 Connection Established
// 之后浏览器和服务器开始 HTTPS 握手并交换加密数据，代理服务器只负责传输彼此的数据包，并不能读取具体数据内容（代理服务器也可以选择安装可信根证书解密 HTTPS 报文）。
// 这个过程其实是通过代理服务器将 HTTP 协议转换为 HTTPS 协议的过程。对 RPC 服务端来，需要做的是将 HTTP 协议转换为 RPC 协议，对客户端来说，需要新增通过 HTTP CONNECT 请求创建连接的逻辑。
// 服务端支持 HTTP 协议
// 客户端向 RPC 服务器发送 CONNECT 请求 CONNECT 10.0.0.1:9999/_zrpc_ HTTP/1.0
// RPC 服务器返回 HTTP 200 状态码表示连接建立。
// HTTP/1.0 200 Connected to ZRPC
// 客户端使用创建好的连接发送 RPC 报文，先发送 Option，再发送 N 个请求报文，服务端处理 RPC 请求并响应。

const (
	connected           = "200 Connected to ZRPC"
	defaultRPCPath      = "/_zrpc_"
	defaultDebugRPCPath = "/debug/_zrpc_" //DEBUG 页面地址。

)

// ServeHTTP implements an http.Handler that answers RPC requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != "CONNECT" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, "405 must CONNECT\n")
		return
	}

	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		log.Print("rpc hijacking", req.RemoteAddr, ": ", err.Error())
		return
	}
	_, _ = io.WriteString(conn, "HTTP/1.0 "+connected+"\n\n")
	s.ServeConn(conn)
}

// HandleHTTP registers an HTTP handler for RPC messages on rpcPath.
// It is still necessary to invoke http.Serve(), typically in a go statement.
func (s *Server) HandleHTTP() {
	http.Handle(defaultRPCPath, s)
	http.Handle(defaultDebugRPCPath, debugHTTP{s})
	log.Println("rpc server debug path:", defaultDebugRPCPath)
}

// HandleHTTP is a convenient approach for default server to register HTTP handlers
func HandleHTTP() {
	DefaultServer.HandleHTTP()
}

// 在 Go 语言中处理 HTTP 请求是非常简单的一件事，Go 标准库中 http.Handle 的实现如下：
// package http
//// Handle registers the handler for the given pattern
//// in the DefaultServeMux.
//// The documentation for ServeMux explains how patterns are matched.
//func Handle(pattern string, handler Handler) { DefaultServeMux.Handle(pattern, handler) }
// 第一个参数是支持通配的字符串 pattern，在这里，我们固定传入 /-_zrpc_，第二个参数是 Handler 类型，Handler 是一个接口类型，定义如下：
// type Handler interface {
//    ServeHTTP(w ResponseWriter, r *Request)
//}
// 只需要实现接口 Handler 即可作为一个 HTTP Handler 处理 HTTP 请求。接口 Handler 只定义了一个方法 ServeHTTP，实现该方法即可。
