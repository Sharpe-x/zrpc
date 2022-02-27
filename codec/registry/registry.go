package registry

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 服务端启动后，向注册中心发送注册消息，注册中心得知该服务已经启动，处于可用状态。一般来说，服务端还需要定期向注册中心发送心跳，证明自己还活着。
//客户端向注册中心询问，当前哪天服务是可用的，注册中心将可用的服务列表返回客户端。
//客户端根据注册中心得到的服务列表，选择其中一个发起调用。

type ZrpcRegistry struct {
	timeout time.Duration
	mu      sync.Mutex
	servers map[string]*ServerItem
}

type ServerItem struct {
	Addr  string
	start time.Time
}

const (
	defaultPath    = "/_zrpc_/registry"
	defaultTimeout = time.Minute * 5
)

var DefaultZrpcRegister = New(defaultTimeout)

func New(timeout time.Duration) *ZrpcRegistry {
	return &ZrpcRegistry{
		servers: make(map[string]*ServerItem),
		timeout: timeout,
	}
}

// putServer 添加服务实例，如果服务已经存在，则更新 start。
func (r *ZrpcRegistry) putServer(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.servers[addr]
	if s == nil {
		r.servers[addr] = &ServerItem{
			Addr:  addr,
			start: time.Now(),
		}
	} else {
		s.start = time.Now()
	}
}

// aliveServers：返回可用的服务列表，如果存在超时的服务，则删除。
func (r *ZrpcRegistry) aliveServers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var alive []string
	for addr, s := range r.servers {
		if r.timeout == 0 || s.start.Add(r.timeout).After(time.Now()) {
			alive = append(alive, addr)
		} else {
			delete(r.servers, addr)
		}
	}
	return alive
}

// 采用 HTTP 协议提供服务，且所有的有用信息都承载在 HTTP Header 中。
// Get：返回所有可用的服务列表，通过自定义字段 X-zrpc-Servers 承载。
// Post：添加服务实例或发送心跳，通过自定义字段 X-zrpc-Server 承载
func (r *ZrpcRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case "GET":
		w.Header().Set("X-Zrpc-Servers", strings.Join(r.aliveServers(), ","))
	case "POST":
		addr := req.Header.Get("X-Zrpc-Server")
		if addr == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		r.putServer(addr)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (r *ZrpcRegistry) HandleHTTP(registryPath string) {
	http.Handle(registryPath, r)
	log.Println("rpc registry path:", registryPath)
}

func HandleHTTP() {
	DefaultZrpcRegister.HandleHTTP(defaultPath)
}

func Heartbeat(registry, addr string, duration time.Duration) {
	if duration == 0 {
		duration = defaultTimeout - time.Duration(1)*time.Minute
	}

	var err error
	err = sendHeartbeat(registry, addr)
	go func() {
		t := time.NewTicker(duration)
		for err == nil {
			<-t.C
			err = sendHeartbeat(registry, addr)
		}
	}()
}

func sendHeartbeat(registry, addr string) error {
	log.Println(addr, "send heart beat to registry", registry)
	httpClient := &http.Client{}
	req, _ := http.NewRequest("POST", registry, nil)
	req.Header.Set("X-Zrpc-Server", addr)
	if _, err := httpClient.Do(req); err != nil {
		log.Println("rpc server: heart beat err: ", err)
		return err
	}
	return nil
}
