package codec

import "io"

/*
	一个典型的RPC调用  err = client.Call("Arith.Multiply",args,&reply)
	客户端发送的请求包括服务名 Arith 方法名 Multiply 参数 args
	服务端的响应包括错误err 返回值reply2个。

	将请求和响应中的参数和返回值抽象为body，剩余的信息放在header 中
*/

// Header rpc 数据结构
type Header struct {
	ServiceMethod string // 服务名和方法名，通常与 Go 语言中的结构体和方法相映射。 format "Service.Method"
	Seq           uint64 // 请求的序号，也可以认为是某个请求的 ID，用来区分不同的请求。
	Error         string // 错误信息，客户端置为空，服务端如果如果发生错误，将错误信息置于 Error 中
}

// Codec 消息编解码接口 抽象接口是为了实现不同的Codec实例
type Codec interface {
	io.Closer // Close() error
	ReadHeader(*Header) error
	ReadBody(interface{}) error
	Write(*Header, interface{}) error
}

// NewCodecFunc 抽象出 Codec 的构造函数，客户端和服务端可以通过 Codec 的 Type 得到构造函数，从而创建 Codec 实例。
type NewCodecFunc func(io.ReadWriteCloser) Codec

type Type string

const (
	GobType  Type = "application/gob"
	JsonType Type = "application/json"
)

var NewCodeFuncMap map[Type]NewCodecFunc

func init() {
	NewCodeFuncMap = make(map[Type]NewCodecFunc)
	NewCodeFuncMap[GobType] = NewGobCodec
}
