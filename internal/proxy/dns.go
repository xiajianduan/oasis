package proxy

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

type DNSDialFunc func(addr string) (net.Conn, error)

type DNSProxy struct {
	localAddr string
	upstreams []string
	dial      DNSDialFunc
	conn      *net.UDPConn
	done      chan struct{}
	wg        sync.WaitGroup
}

func NewDNSProxy(upstreams []string, dial DNSDialFunc) *DNSProxy {
	if dial == nil {
		dial = func(addr string) (net.Conn, error) {
			return net.DialTimeout("tcp", addr, 5*time.Second)
		}
	}
	return &DNSProxy{
		upstreams: upstreams,
		dial:      dial,
	}
}

func (d *DNSProxy) Start() error {
	udpAddr, err := net.ResolveUDPAddr("udp", "0.0.0.0:0")
	if err != nil {
		return fmt.Errorf("解析 DNS 地址失败: %w", err)
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("监听 DNS 失败: %w", err)
	}
	d.conn = conn
	d.localAddr = conn.LocalAddr().String()
	d.done = make(chan struct{})

	log.Printf("[dns] 本地 DNS 代理已启动 %s", d.localAddr)

	d.wg.Add(1)
	go d.serve()
	return nil
}

func (d *DNSProxy) Addr() string {
	return d.localAddr
}

func (d *DNSProxy) serve() {
	defer d.wg.Done()
	buf := make([]byte, 65536)
	for {
		select {
		case <-d.done:
			return
		default:
		}

		d.conn.SetReadDeadline(time.Now().Add(time.Second))
		n, clientAddr, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-d.done:
				return
			default:
				log.Printf("[dns] 读取错误: %v", err)
				continue
			}
		}

		query := make([]byte, n)
		copy(query, buf[:n])

		d.wg.Add(1)
		go d.forward(query, clientAddr)
	}
}

func (d *DNSProxy) forward(query []byte, clientAddr *net.UDPAddr) {
	defer d.wg.Done()
	for _, upstream := range d.upstreams {
		resp, err := d.exchangeTCP(upstream, query)
		if err != nil {
			log.Printf("[dns] TCP 查询 %s 失败: %v", upstream, err)
			continue
		}
		d.conn.WriteTo(resp, clientAddr)
		return
	}
	log.Printf("[dns] 所有上游 DNS 查询失败")
}

func (d *DNSProxy) exchangeTCP(upstream string, query []byte) ([]byte, error) {
	addr := net.JoinHostPort(upstream, "53")
	conn, err := d.dial(addr)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败: %w", addr, err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second))

	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(len(query)))
	if _, err := conn.Write(length[:]); err != nil {
		return nil, fmt.Errorf("发送 DNS 长度失败: %w", err)
	}
	if _, err := conn.Write(query); err != nil {
		return nil, fmt.Errorf("发送 DNS 查询失败: %w", err)
	}

	if _, err := conn.Read(length[:]); err != nil {
		return nil, fmt.Errorf("读取响应长度失败: %w", err)
	}
	respLen := binary.BigEndian.Uint16(length[:])
	resp := make([]byte, respLen)
	var total int
	for total < int(respLen) {
		n, err := conn.Read(resp[total:])
		if err != nil {
			return nil, fmt.Errorf("读取 DNS 响应失败: %w", err)
		}
		total += n
	}
	return resp, nil
}

func (d *DNSProxy) Close() {
	if d.done != nil {
		close(d.done)
	}
	if d.conn != nil {
		d.conn.Close()
	}
	d.wg.Wait()
}
