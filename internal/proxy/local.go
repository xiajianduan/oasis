package proxy

import (
	"log"
	"net"
	"sync"

	"github.com/oasis/oasis/internal/config"
	"github.com/oasis/oasis/internal/router"
)

// LocalProxy 本地 SOCKS5 代理，按规则分流
type LocalProxy struct {
	listener net.Listener
	cfg      *config.Config
	router   *router.Router
	wg       sync.WaitGroup
	done     chan struct{}
}

// NewLocalProxy 创建本地 SOCKS5 代理
func NewLocalProxy(cfg *config.Config) (*LocalProxy, error) {
	rules, err := config.ParseRules(cfg.Rules)
	if err != nil {
		return nil, err
	}
	return &LocalProxy{
		cfg:    cfg,
		router: router.New(rules),
		done:   make(chan struct{}),
	}, nil
}

// Start 在随机端口启动本地 SOCKS5 代理
func (p *LocalProxy) Start() error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	p.listener = listener
	log.Printf("[proxy] 本地 SOCKS5 代理已启动: %s", listener.Addr())

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-p.done:
					return
				default:
					continue
				}
			}
			go p.handle(conn)
		}
	}()
	return nil
}

// Addr 返回本地代理地址 (127.0.0.1:端口)
func (p *LocalProxy) Addr() string {
	if p.listener != nil {
		return p.listener.Addr().String()
	}
	return ""
}

// Close 停止本地代理
func (p *LocalProxy) Close() {
	close(p.done)
	if p.listener != nil {
		p.listener.Close()
	}
	p.wg.Wait()
}

func (p *LocalProxy) handle(client net.Conn) {
	defer client.Close()

	target, err := SOCKS5Handshake(client)
	if err != nil {
		return
	}

	action := p.router.Match(target)
	log.Printf("[proxy] %s -> %s", target, action)

	switch action {
	case "DIRECT":
		remote, err := net.Dial("tcp", target)
		if err != nil {
			SOCKS5Reply(client, false)
			return
		}
		defer remote.Close()
		SOCKS5Reply(client, true)
		Relay(client, remote)

	case "PROXY":
		node, err := p.cfg.GetSelectedNode()
		if err != nil {
			SOCKS5Reply(client, false)
			return
		}
		remote, err := DialUpstream(node, target)
		if err != nil {
			SOCKS5Reply(client, false)
			return
		}
		defer remote.Close()
		SOCKS5Reply(client, true)
		Relay(client, remote)

	case "REJECT":
		SOCKS5Reply(client, false)

	default:
		SOCKS5Reply(client, false)
	}
}
