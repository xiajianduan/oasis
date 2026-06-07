package proxy

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/oasis/oasis/internal/config"
	"github.com/oasis/oasis/internal/router"
)

type HTTPProxy struct {
	listener net.Listener
	cfg      *config.Config
	router   *router.Router
	wg       sync.WaitGroup
	done     chan struct{}
}

func NewHTTPProxy(cfg *config.Config) (*HTTPProxy, error) {
	rules, err := config.ParseRules(cfg.Rules)
	if err != nil {
		return nil, err
	}
	return &HTTPProxy{
		cfg:    cfg,
		router: router.New(rules),
		done:   make(chan struct{}),
	}, nil
}

func (p *HTTPProxy) Start() error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	p.listener = listener
	log.Printf("[http-proxy] 本地 HTTP 代理已启动: %s", listener.Addr())

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

func (p *HTTPProxy) Addr() string {
	if p.listener != nil {
		return p.listener.Addr().String()
	}
	return ""
}

func (p *HTTPProxy) Close() {
	close(p.done)
	if p.listener != nil {
		p.listener.Close()
	}
	p.wg.Wait()
}

func (p *HTTPProxy) handle(client net.Conn) {
	defer client.Close()

	reader := bufio.NewReader(client)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}

	target, err := httpTarget(req)
	if err != nil {
		http.Error(responseWriter{client}, err.Error(), http.StatusBadRequest)
		return
	}

	action := "PROXY"
	if p.cfg.Mode == "rule" {
		action = p.router.Match(target)
	}
	log.Printf("[http-proxy] %s %s -> %s", req.Method, target, action)

	var remote net.Conn
	switch action {
	case "DIRECT":
		remote, err = net.Dial("tcp", target)
	case "PROXY":
		var node *config.NodeConfig
		node, err = p.cfg.GetSelectedNode()
		if err == nil {
			remote, err = DialUpstream(node, target)
		}
	case "REJECT":
		err = fmt.Errorf("rejected")
	default:
		err = fmt.Errorf("unknown action: %s", action)
	}
	if err != nil {
		http.Error(responseWriter{client}, err.Error(), http.StatusBadGateway)
		return
	}
	defer remote.Close()

	if req.Method == http.MethodConnect {
		_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		RelayWithBuffered(client, remote, reader)
		return
	}

	req.RequestURI = ""
	req.URL.Scheme = ""
	req.URL.Host = ""
	req.Header.Del("Proxy-Connection")
	if err := req.Write(remote); err != nil {
		return
	}
	RelayWithBuffered(client, remote, reader)
}

func httpTarget(req *http.Request) (string, error) {
	host := req.Host
	if req.URL != nil && req.URL.Host != "" {
		host = req.URL.Host
	}
	if host == "" {
		return "", fmt.Errorf("missing host")
	}
	if strings.Contains(host, ":") {
		return host, nil
	}
	if req.Method == http.MethodConnect {
		return net.JoinHostPort(host, "443"), nil
	}
	return net.JoinHostPort(host, "80"), nil
}

func RelayWithBuffered(client net.Conn, remote net.Conn, reader *bufio.Reader) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		if reader.Buffered() > 0 {
			_, _ = io.CopyN(remote, reader, int64(reader.Buffered()))
		}
		_, _ = io.Copy(remote, client)
		wg.Done()
	}()
	go func() {
		_, _ = io.Copy(client, remote)
		wg.Done()
	}()
	wg.Wait()
}

type responseWriter struct {
	conn net.Conn
}

func (w responseWriter) Header() http.Header {
	return http.Header{}
}

func (w responseWriter) Write(data []byte) (int, error) {
	return w.conn.Write(data)
}

func (w responseWriter) WriteHeader(statusCode int) {
	_, _ = fmt.Fprintf(w.conn, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n", statusCode, http.StatusText(statusCode))
}
