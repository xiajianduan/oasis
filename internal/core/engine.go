package core

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/oasis/oasis/internal/assets"
	"github.com/oasis/oasis/internal/config"
	"github.com/oasis/oasis/internal/proxy"
	"github.com/oasis/oasis/internal/systemproxy"
	"github.com/oasis/oasis/internal/tun"
)

const libexecTun2socks = "/usr/local/libexec/oasis-tun2socks"

// Engine 代理引擎，管理 TUN 模式
type Engine struct {
	cfg *config.Config

	mu         sync.Mutex
	running    bool
	cancel     context.CancelFunc
	tun2socks  *exec.Cmd
	localProxy *proxy.LocalProxy // rule 模式下的本地 SOCKS5 代理
	dnsProxy   *proxy.DNSProxy   // 本地 DNS 转发器 (UDP → TCP)
	httpProxy  *proxy.HTTPProxy  // 系统 HTTP/HTTPS 代理入口

	txBytes atomic.Int64
	rxBytes atomic.Int64
}

// New 创建引擎
func New(cfg *config.Config) (*Engine, error) {
	return &Engine{
		cfg: cfg,
	}, nil
}

// Start 启动引擎
func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.startLocked()
}

func (e *Engine) startLocked() error {
	if e.running {
		return fmt.Errorf("代理已在运行中")
	}

	if e.cfg.Mode == "direct" {
		e.running = true
		log.Printf("[oasis] direct 模式已启动（无代理）")
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel

	if e.cfg.Mode == "rule" {
		lp, err := proxy.NewLocalProxy(e.cfg)
		if err != nil {
			cancel()
			return fmt.Errorf("创建本地代理失败: %w", err)
		}
		if err := lp.Start(); err != nil {
			cancel()
			return fmt.Errorf("启动本地代理失败: %w", err)
		}
		e.localProxy = lp
	}

	if e.cfg.TUN.Enabled {
		dnsDial := e.dnsDialer()
		dnsProxy := proxy.NewDNSProxy(e.cfg.TUN.DNS, dnsDial)
		if err := dnsProxy.Start(); err != nil {
			cancel()
			if e.localProxy != nil {
				e.localProxy.Close()
			}
			return fmt.Errorf("启动 DNS 代理失败: %w", err)
		}
		e.dnsProxy = dnsProxy

		_, dnsPort, _ := net.SplitHostPort(dnsProxy.Addr())

		if err := tun.EnableDNSRedirect(dnsPort); err != nil {
			cancel()
			dnsProxy.Close()
			if e.localProxy != nil {
				e.localProxy.Close()
			}
			return fmt.Errorf("DNS 重定向失败: %w", err)
		}

		if err := tun.AddDNS([]string{"127.0.0.1"}); err != nil {
			log.Printf("[oasis] 设置系统 DNS 失败: %v", err)
		}

		go func() {
			if err := e.startTUN(ctx); err != nil {
				log.Printf("[oasis] TUN 启动失败: %v", err)
			}
		}()
	}

	if e.cfg.SystemProxy.Enabled {
		httpProxy, err := proxy.NewHTTPProxy(e.cfg)
		if err != nil {
			cancel()
			e.closeStartedProxies()
			return fmt.Errorf("创建 HTTP 系统代理失败: %w", err)
		}
		if err := httpProxy.Start(); err != nil {
			cancel()
			e.closeStartedProxies()
			return fmt.Errorf("启动 HTTP 系统代理失败: %w", err)
		}
		e.httpProxy = httpProxy
		if err := e.enableSystemProxy(); err != nil {
			cancel()
			e.closeStartedProxies()
			return fmt.Errorf("设置系统代理失败: %w", err)
		}
	}

	log.Printf("[oasis] %s 模式已启动 (TUN: %v, 系统代理: %v)", e.cfg.Mode, e.cfg.TUN.Enabled, e.cfg.SystemProxy.Enabled)
	e.running = true
	return nil
}

// Stop 停止引擎
func (e *Engine) Stop() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopLocked()
}

func (e *Engine) stopLocked() error {
	if !e.running {
		return fmt.Errorf("代理未在运行")
	}

	if e.cfg.SystemProxy.Enabled {
		systemproxy.Disable()
	}

	if e.cancel != nil {
		e.cancel()
	}

	if e.tun2socks != nil && e.tun2socks.Process != nil {
		log.Printf("[oasis] 停止 tun2socks 进程 (PID: %d)", e.tun2socks.Process.Pid)
		e.tun2socks.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- e.tun2socks.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			e.tun2socks.Process.Kill()
		}
		e.tun2socks = nil
	}

	if e.cfg.TUN.Enabled {
		sudoRoute("delete", "-net", "0.0.0.0", "-netmask", "128.0.0.0")
		sudoRoute("delete", "-net", "128.0.0.0", "-netmask", "128.0.0.0")
	}

	if e.dnsProxy != nil {
		tun.RestoreDNS()
	}

	if e.localProxy != nil {
		e.localProxy.Close()
		e.localProxy = nil
	}

	if e.httpProxy != nil {
		e.httpProxy.Close()
		e.httpProxy = nil
	}

	if e.dnsProxy != nil {
		e.dnsProxy.Close()
		e.dnsProxy = nil
	}

	if e.cfg.TUN.Enabled {
		tun.DisableDNSRedirect()
	}

	e.running = false
	log.Printf("[oasis] 代理已停止")
	return nil
}

// IsRunning 是否在运行
func (e *Engine) IsRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// Mode 获取当前模式
func (e *Engine) Mode() string {
	return e.cfg.Mode
}

// Reload 重载配置（支持模式切换）
func (e *Engine) Reload(cfg *config.Config) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.running {
		return fmt.Errorf("代理未在运行，请先启动")
	}

	oldMode := e.cfg.Mode
	newMode := cfg.Mode
	oldSystemProxyEnabled := e.cfg.SystemProxy.Enabled

	if oldMode == newMode && (oldSystemProxyEnabled || cfg.SystemProxy.Enabled) {
		if err := e.stopLocked(); err != nil {
			return err
		}
		e.cfg = cfg
		return e.startLocked()
	}

	if oldMode == newMode {
		log.Printf("[oasis] 配置已重载")
		e.cfg = cfg
		return nil
	}

	log.Printf("[oasis] 切换模式: %s → %s", oldMode, newMode)

	// 先停止旧模式（用旧配置清理）
	if err := e.stopLocked(); err != nil {
		return err
	}

	// 更新配置
	e.cfg = cfg

	// 再用新模式启动
	return e.startLocked()
}

func (e *Engine) enableSystemProxy() error {
	if e.httpProxy == nil {
		return fmt.Errorf("HTTP 系统代理未启动")
	}
	httpHost, httpPort, err := proxy.ParseAddr(e.httpProxy.Addr())
	if err != nil {
		return err
	}

	if e.cfg.Mode == "rule" && e.localProxy != nil {
		host, port, err := proxy.ParseAddr(e.localProxy.Addr())
		if err != nil {
			return err
		}
		return systemproxy.Enable(host, port, httpHost, httpPort)
	}

	node, err := e.cfg.GetSelectedNode()
	if err != nil {
		return err
	}
	if node.Type != "socks5" {
		return fmt.Errorf("系统代理仅能直接指向 SOCKS5 节点，当前节点类型: %s；请切换到 rule 模式使用本地 SOCKS 入口", node.Type)
	}
	return systemproxy.Enable(node.Server, node.Port, httpHost, httpPort)
}

func (e *Engine) closeStartedProxies() {
	if e.httpProxy != nil {
		e.httpProxy.Close()
		e.httpProxy = nil
	}
	if e.dnsProxy != nil {
		tun.RestoreDNS()
		e.dnsProxy.Close()
		e.dnsProxy = nil
	}
	if e.localProxy != nil {
		e.localProxy.Close()
		e.localProxy = nil
	}
}

func (e *Engine) dnsDialer() proxy.DNSDialFunc {
	if e.cfg.Mode == "direct" {
		return nil
	}

	node, err := e.cfg.GetSelectedNode()
	if err != nil {
		log.Printf("[dns] 未找到当前节点，DNS 查询将直连: %v", err)
		return nil
	}

	return func(addr string) (net.Conn, error) {
		return proxy.DialUpstream(node, addr)
	}
}

// Stats 返回流量统计
func (e *Engine) Stats() (tx, rx int64) {
	return e.txBytes.Load(), e.rxBytes.Load()
}

// listUtunDevices 返回当前所有 utun 设备名（前后两次检测用）
func listUtunDevices() []string {
	out, err := exec.Command("ifconfig").Output()
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "utun") {
			continue
		}
		name := strings.SplitN(line, ":", 2)[0]
		names = append(names, name)
	}
	return names
}

// findNewUtunDevice 在已有设备列表之外，查找新创建的未配 IP 的 utun 设备
func findNewUtunDevice(existing []string) string {
	out, err := exec.Command("ifconfig").Output()
	if err != nil {
		return ""
	}
	existingSet := make(map[string]bool, len(existing))
	for _, n := range existing {
		existingSet[n] = true
	}

	lines := strings.Split(string(out), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "utun") {
			continue
		}
		name := strings.SplitN(line, ":", 2)[0]
		if existingSet[name] {
			continue
		}
		hasIP := false
		for j := i + 1; j < len(lines) && len(lines[j]) > 0 && lines[j][0] == '\t'; j++ {
			if strings.Contains(lines[j], "inet ") && !strings.Contains(lines[j], "inet6") {
				hasIP = true
				break
			}
		}
		if !hasIP {
			return name
		}
	}
	return ""
}

func (e *Engine) startTUN(ctx context.Context) error {
	tunCfg := e.cfg.TUN

	existingUtuns := listUtunDevices()

	tun2socksBin := findTun2socks()
	if tun2socksBin == "" {
		return fmt.Errorf("未找到 tun2socks 二进制")
	}

	var proxyAddr string
	if e.cfg.Mode == "rule" && e.localProxy != nil {
		proxyAddr = "socks5://" + e.localProxy.Addr()
	} else {
		proxyAddr = tunCfg.Via
		if proxyAddr == "" {
			if node, err := e.cfg.GetSelectedNode(); err == nil {
				proxyAddr = fmt.Sprintf("socks5://%s:%d", node.Server, node.Port)
			}
		}
	}
	if proxyAddr == "" {
		return fmt.Errorf("TUN 模式未配置代理地址")
	}

	mtu := tunCfg.MTU
	if mtu == 0 {
		mtu = 1500
	}

	tunArgs := []string{
		"-device", "tun://utun",
		"-proxy", proxyAddr,
		"-loglevel", "info",
		"-udp-timeout", "30s",
	}

	if tunCfg.MTU > 0 {
		tunArgs = append(tunArgs, "-mtu", fmt.Sprintf("%d", tunCfg.MTU))
	}

	// 收集需要绕过 TUN 的 IP（避免路由环路）
	var bypassIPs []net.IP
	addBypass := func(raw string) {
		raw = strings.TrimPrefix(raw, "socks5://")
		raw = strings.TrimPrefix(raw, "http://")
		if h, _, err := net.SplitHostPort(raw); err == nil {
			if ip := net.ParseIP(h); ip != nil {
				bypassIPs = append(bypassIPs, ip)
			}
		}
	}
	if e.cfg.Mode == "rule" && e.localProxy != nil {
		if node, err := e.cfg.GetSelectedNode(); err == nil {
			addBypass(fmt.Sprintf("%s:%d", node.Server, node.Port))
		}
	} else {
		if tunCfg.Via != "" {
			addBypass(tunCfg.Via)
		} else if node, err := e.cfg.GetSelectedNode(); err == nil {
			addBypass(fmt.Sprintf("%s:%d", node.Server, node.Port))
		}
	}

	// 启动 tun2socks（sudoer 已注册，免密）
	var cmd *exec.Cmd
	if os.Geteuid() != 0 {
		cmd = exec.CommandContext(ctx, "sudo", "-n", tun2socksBin)
	} else {
		cmd = exec.CommandContext(ctx, tun2socksBin)
	}
	cmd.Args = append(cmd.Args, tunArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	e.mu.Lock()
	e.tun2socks = cmd
	e.mu.Unlock()

	log.Printf("[tun] 启动 tun2socks")

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 tun2socks 失败: %w", err)
	}

	// 等待并检测新 utun 设备
	var tunDev string
	for i := 0; i < 30; i++ {
		time.Sleep(200 * time.Millisecond)
		tunDev = findNewUtunDevice(existingUtuns)
		if tunDev != "" {
			break
		}
	}
	if tunDev == "" {
		return fmt.Errorf("未检测到新创建的 TUN 设备")
	}
	log.Printf("[tun] 检测到 TUN 设备: %s", tunDev)

	// 配置 IP（sudoer 已注册，免密）
	sudoRoute("ifconfig", tunDev, "10.0.0.1", "10.0.0.1", "up")

	// 代理节点走原默认网关，避免 TUN 默认路由生效后把上游连接绕回 TUN。
	for _, ip := range bypassIPs {
		gateway := defaultGateway()
		if gateway == "" {
			log.Printf("[tun] 未找到默认网关，跳过节点直连路由: %s", ip)
			continue
		}
		log.Printf("[tun] 添加节点直连路由: %s -> %s", ip, gateway)
		sudoRoute("route", "-n", "delete", "-host", ip.String())
		sudoRoute("route", "-n", "add", "-host", ip.String(), gateway)
	}

	// 配置路由（sudoer 已注册，免密）
	sudoRoute("route", "-n", "add", "-net", "0.0.0.0", "-netmask", "128.0.0.0", "-interface", tunDev)
	sudoRoute("route", "-n", "add", "-net", "128.0.0.0", "-netmask", "128.0.0.0", "-interface", tunDev)

	go func() {
		<-ctx.Done()
		if cmd.Process != nil {
			cmd.Process.Signal(syscall.SIGTERM)
		}
	}()

	err := cmd.Wait()
	if err != nil {
		select {
		case <-ctx.Done():
			log.Printf("[tun] tun2socks 已停止")
			return nil
		default:
			return fmt.Errorf("tun2socks 异常退出: %w", err)
		}
	}

	return nil
}

// defaultGateway 返回当前默认网关 IP。
func defaultGateway() string {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "gateway:") {
			if gateway := strings.TrimSpace(strings.TrimPrefix(line, "gateway:")); gateway != "" {
				return gateway
			}
		}
	}
	return ""
}

func sudoRoute(args ...string) {
	if os.Geteuid() != 0 {
		args = append([]string{"-n"}, args...)
		exec.Command("sudo", args...).Run()
	} else {
		exec.Command(args[0], args[1:]...).Run()
	}
}

func findTun2socks() string {
	// 优先查固定路径（/usr/local/libexec/oasis-tun2socks 在 sudoers 中注册了免密）
	if _, err := os.Stat(libexecTun2socks); err == nil {
		return libexecTun2socks
	}

	if p, err := assets.Tun2socksPath(); err == nil {
		return p
	}

	execPath, err := os.Executable()
	if err == nil {
		binDir := filepath.Join(filepath.Dir(execPath), "bin", "tun2socks")
		if _, err := os.Stat(binDir); err == nil {
			return binDir
		}
	}

	cwd, _ := os.Getwd()
	localBin := filepath.Join(cwd, "bin", "tun2socks")
	if _, err := os.Stat(localBin); err == nil {
		return localBin
	}

	if p, err := exec.LookPath("tun2socks"); err == nil {
		return p
	}

	return ""
}

func FormatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
