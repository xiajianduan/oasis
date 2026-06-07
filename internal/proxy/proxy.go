package proxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"sync"

	"github.com/oasis/oasis/internal/config"
)

var DialTimeout = 10 * time.Second

// SOCKS5 协议常量
const (
	SOCKS5Version = 0x05
	CmdConnect    = 0x01
	AtypIPv4      = 0x01
	AtypDomain    = 0x03
	AtypIPv6      = 0x04
	RepSuccess    = 0x00
	RepFailure    = 0x01
)

// SOCKS5Handshake 处理 SOCKS5 握手，返回目标地址
func SOCKS5Handshake(conn net.Conn) (string, error) {
	buf := make([]byte, 263)

	// 1. 读取认证方法
	n, err := conn.Read(buf)
	if err != nil || n < 3 {
		return "", errors.New("读取认证方法失败")
	}

	if buf[0] != SOCKS5Version {
		return "", errors.New("不支持的 SOCKS 版本")
	}

	// 回复：无需认证
	if _, err := conn.Write([]byte{SOCKS5Version, 0x00}); err != nil {
		return "", err
	}

	// 2. 读取请求
	n, err = conn.Read(buf)
	if err != nil || n < 10 {
		return "", errors.New("读取请求失败")
	}

	if buf[0] != SOCKS5Version || buf[1] != CmdConnect {
		return "", errors.New("仅支持 CONNECT 命令")
	}

	// 解析目标地址
	var target string
	switch buf[3] {
	case AtypIPv4:
		if n < 10 {
			return "", errors.New("IPv4 地址不完整")
		}
		ip := net.IP(buf[4:8])
		port := int(buf[8])<<8 | int(buf[9])
		target = fmt.Sprintf("%s:%d", ip.String(), port)
	case AtypDomain:
		domainLen := int(buf[4])
		if n < 5+domainLen+2 {
			return "", errors.New("域名不完整")
		}
		domain := string(buf[5 : 5+domainLen])
		port := int(buf[5+domainLen])<<8 | int(buf[5+domainLen+1])
		target = fmt.Sprintf("%s:%d", domain, port)
	case AtypIPv6:
		if n < 22 {
			return "", errors.New("IPv6 地址不完整")
		}
		ip := net.IP(buf[4:20])
		port := int(buf[20])<<8 | int(buf[21])
		target = fmt.Sprintf("[%s]:%d", ip.String(), port)
	default:
		return "", errors.New("不支持的地址类型")
	}

	return target, nil
}

// SOCKS5Reply 发送 SOCKS5 响应
func SOCKS5Reply(conn net.Conn, success bool) error {
	rep := byte(RepFailure)
	if success {
		rep = RepSuccess
	}
	// 回复格式: VER, REP, RSV, ATYP, BND.ADDR(4), BND.PORT(2)
	reply := []byte{SOCKS5Version, rep, 0x00, AtypIPv4, 0, 0, 0, 0, 0, 0}
	_, err := conn.Write(reply)
	return err
}

// DialUpstream 通过上游节点建立连接
func DialUpstream(node *config.NodeConfig, target string) (net.Conn, error) {
	switch node.Type {
	case "socks5":
		return dialSOCKS5Upstream(node, target)
	case "shadowsocks":
		return dialShadowsocksUpstream(node, target)
	default:
		return nil, fmt.Errorf("不支持的节点类型: %s", node.Type)
	}
}

// dialSOCKS5Upstream 通过 SOCKS5 上游连接目标
func dialSOCKS5Upstream(node *config.NodeConfig, target string) (net.Conn, error) {
	addr := fmt.Sprintf("%s:%d", node.Server, node.Port)
	conn, err := net.DialTimeout("tcp", addr, DialTimeout)
	if err != nil {
		return nil, fmt.Errorf("连接上游 SOCKS5 %s 失败: %w", addr, err)
	}

	// 解析目标地址
	host, port, err := ParseAddr(target)
	if err != nil {
		conn.Close()
		return nil, err
	}

	// SOCKS5 握手
	if err := socks5Connect(conn, host, port, node.Username, node.Password); err != nil {
		conn.Close()
		return nil, fmt.Errorf("上游 SOCKS5 握手失败: %w", err)
	}

	return conn, nil
}

// dialShadowsocksUpstream 通过 Shadowsocks 上游连接目标
func dialShadowsocksUpstream(node *config.NodeConfig, target string) (net.Conn, error) {
	addr := fmt.Sprintf("%s:%d", node.Server, node.Port)
	conn, err := net.DialTimeout("tcp", addr, DialTimeout)
	if err != nil {
		return nil, fmt.Errorf("连接上游 SS %s 失败: %w", addr, err)
	}

	host, port, err := ParseAddr(target)
	if err != nil {
		conn.Close()
		return nil, err
	}

	// 构建目标地址
	addrBytes := buildAddrBytes(host, port)

	// 用 Shadowsocks 加密
	cipher, err := NewStreamCipher(node.Method, node.Password)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("创建 SS 加密器失败: %w", err)
	}

	ssConn := NewSSConn(conn, cipher, node.Password)
	if _, err := ssConn.Write(addrBytes); err != nil {
		ssConn.Close()
		return nil, fmt.Errorf("SS 发送目标地址失败: %w", err)
	}

	return ssConn, nil
}

// socks5Connect 向上游 SOCKS5 发起连接请求
func socks5Connect(conn net.Conn, host string, port int, username, password string) error {
	// 认证协商
	authMethods := []byte{SOCKS5Version, 1, 0x00} // 无认证
	if username != "" {
		authMethods = []byte{SOCKS5Version, 1, 0x02} // 用户名/密码
	}
	if _, err := conn.Write(authMethods); err != nil {
		return err
	}

	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return err
	}

	if resp[1] == 0x02 {
		// 用户名/密码认证
		auth := []byte{0x01}
		auth = append(auth, byte(len(username)))
		auth = append(auth, []byte(username)...)
		auth = append(auth, byte(len(password)))
		auth = append(auth, []byte(password)...)
		if _, err := conn.Write(auth); err != nil {
			return err
		}
		authResp := make([]byte, 2)
		if _, err := io.ReadFull(conn, authResp); err != nil {
			return err
		}
		if authResp[1] != 0x00 {
			return errors.New("上游 SOCKS5 认证失败")
		}
	}

	// 发送 CONNECT 请求
	addrBytes := buildAddrBytes(host, port)
	req := append([]byte{SOCKS5Version, CmdConnect, 0x00}, addrBytes...)
	if _, err := conn.Write(req); err != nil {
		return err
	}

	// 读取响应
	resp = make([]byte, 10)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return err
	}

	if resp[1] != RepSuccess {
		return fmt.Errorf("上游 SOCKS5 连接失败, 错误码: %d", resp[1])
	}

	// 根据地址类型跳过剩余地址字节
	switch resp[3] {
	case AtypIPv4:
		// 已读取 10 字节，无需额外读取
	case AtypDomain:
		extra := make([]byte, resp[4])
		io.ReadFull(conn, extra)
	case AtypIPv6:
		extra := make([]byte, 12)
		io.ReadFull(conn, extra)
	}

	return nil
}

// buildAddrBytes 构建 SOCKS5 地址字节
func buildAddrBytes(host string, port int) []byte {
	ip := net.ParseIP(host)
	if ip != nil {
		if v4 := ip.To4(); v4 != nil {
			addr := []byte{AtypIPv4}
			addr = append(addr, v4...)
			addr = append(addr, byte(port>>8), byte(port))
			return addr
		}
		addr := []byte{AtypIPv6}
		addr = append(addr, ip.To16()...)
		addr = append(addr, byte(port>>8), byte(port))
		return addr
	}

	addr := []byte{AtypDomain, byte(len(host))}
	addr = append(addr, []byte(host)...)
	addr = append(addr, byte(port>>8), byte(port))
	return addr
}

// ParseAddr 解析 "host:port" 格式地址
func ParseAddr(addr string) (host string, port int, err error) {
	h, p, e := net.SplitHostPort(addr)
	if e != nil {
		return "", 0, e
	}
	host = h
	fmt.Sscanf(p, "%d", &port)
	return
}

// Relay 双向转发数据（等待两个方向都完成）
func Relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		io.Copy(b, a)
		wg.Done()
	}()
	go func() {
		io.Copy(a, b)
		wg.Done()
	}()
	wg.Wait()
}
