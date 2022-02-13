package zrpc

import (
	"reflect"
	"sync/atomic"
)

// 结构体映射为服务
// RPC 框架的一个基础能力是：像调用本地程序一样调用远程服务。那如何将程序映射为服务呢？那么对 Go 来说，这个问题就变成了如何将结构体的方法映射为服务。
// 对 net/rpc 而言，一个函数需要能够被远程调用，需要满足如下五个条件：
//
//the method’s type is exported. – 方法所属类型是导出的。
//the method is exported. – 方式是导出的。
//the method has two arguments, both exported (or builtin) types. – 两个入参，均为导出或内置类型。
//the method’s second argument is a pointer. – 第二个入参必须是一个指针。
//the method has return type error. – 返回值为 error 类型。
// example func (t *T) MethodName(argType T1, replyType *T2) error
// 假设客户端发过来一个请求，包含 ServiceMethod 和 Argv。
// {
//    "ServiceMethod"： "T.MethodName"
//    "Argv"："0101110101..." // 序列化之后的字节流
//}
// 通过 “T.MethodName” 可以确定调用的是类型 T 的 MethodName，如果硬编码实现这个功能，很可能是这样：
//  switch req.ServiceMethod {
//    case "T.MethodName":
//        t := new(t)
//        reply := new(T2)
//        var argv T1
//        gob.NewDecoder(conn).Decode(&argv)
//        err := t.MethodName(argv, reply)
//        server.sendMessage(reply, err)
//    case "Foo.Sum":
//        f := new(Foo)
//        ...
//}
// 使用硬编码的方式来实现结构体与服务的映射，那么每暴露一个方法，就需要编写等量的代码。那有没有什么方式，能够将这个映射过程自动化呢？可以借助反射。
//
//通过反射，我们能够非常容易地获取某个结构体的所有方法，并且能够通过方法，获取到该方法所有的参数类型与返回值。例如：

type methodType struct {
	method    reflect.Method // 方法本身
	ArgType   reflect.Type   // 第一个参数的类型
	ReplyType reflect.Type   // 第二个参数的类型
	numCalls  uint64         // 统计方法调用次数
}

func (m *methodType) NumCalls() uint64 {
	return atomic.LoadUint64(&m.numCalls)
}

func (m *methodType) newArgv() reflect.Value {
	var argv reflect.Value
	// arg may be a pointer type, or a value type
	if m.ArgType.Kind() == reflect.Ptr {
		argv = reflect.New(m.ArgType.Elem())
	} else {
		argv = reflect.New(m.ArgType).Elem()
	}
	return argv
}

func (m *methodType) newReplyv() reflect.Value {
	// reply must be a pointer type
	replyv := reflect.New(m.ReplyType.Elem())
	switch m.ReplyType.Elem().Kind() {
	case reflect.Map:
		replyv.Elem().Set(reflect.MakeMap(m.ReplyType.Elem()))
	case reflect.Slice:
		replyv.Elem().Set(reflect.MakeSlice(m.ReplyType.Elem(), 0, 0))
	}
	return replyv
}
