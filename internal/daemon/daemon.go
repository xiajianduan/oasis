package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/oasis/oasis/internal/config"
	"github.com/oasis/oasis/internal/core"
)

const (
	socketName = "oasis.sock"
	pidName    = "oasis.pid"
)

// Daemon 后台守护进程管理
type Daemon struct {
	engine     *core.Engine
	cfg        *config.Config
	socketPath string
	pidPath    string
}

// New 创建守护进程
func New(cfg *config.Config) (*Daemon, error) {
	engine, err := core.New(cfg)
	if err != nil {
		return nil, err
	}

	oasisDir := config.Dir()

	return &Daemon{
		engine:     engine,
		cfg:        cfg,
		socketPath: filepath.Join(oasisDir, socketName),
		pidPath:    filepath.Join(oasisDir, pidName),
	}, nil
}

// Start 启动守护进程
func (d *Daemon) Start() error {
	// 检查是否已在运行
	if IsRunning(d.socketPath) {
		return fmt.Errorf("oasis 已在运行中")
	}

	// 确保目录存在
	oasisDir := config.Dir()
	os.MkdirAll(oasisDir, 0755)

	// 设置日志输出到文件
	logPath := filepath.Join(oasisDir, "oasis.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, logFile))
		defer logFile.Close()
	}

	// 启动代理引擎
	if err := d.engine.Start(); err != nil {
		return err
	}

	// 写入 PID
	pid := os.Getpid()
	pidData, _ := json.Marshal(map[string]int{"pid": pid})
	os.WriteFile(d.pidPath, pidData, 0644)
	os.Chmod(d.pidPath, 0644)

	// 创建 Unix Socket 用于 IPC
	os.Remove(d.socketPath)
	l, err := net.Listen("unix", d.socketPath)
	if err != nil {
		return fmt.Errorf("创建 socket 失败: %w", err)
	}

	// 设置 socket 文件权限为 0666，允许所有用户连接
	os.Chmod(d.socketPath, 0666)

	log.Printf("[daemon] 守护进程已启动, PID: %d", pid)

	// 监听信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				select {
				case <-sigCh:
					return
				default:
					continue
				}
			}
			go d.handleIPC(conn)
		}
	}()

	<-sigCh
	log.Printf("[daemon] 收到停止信号")

	l.Close()
	d.engine.Stop()
	os.Remove(d.socketPath)
	os.Remove(d.pidPath)

	return nil
}

// Stop 停止守护进程
func Stop() error {
	oasisDir := config.Dir()
	socketPath := filepath.Join(oasisDir, socketName)
	pidPath := filepath.Join(oasisDir, pidName)

	if !IsRunning(socketPath) {
		return fmt.Errorf("oasis 未在运行")
	}

	// 发送停止命令
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		// 如果 socket 连接失败，尝试通过 PID 杀进程
		pidData, err := os.ReadFile(pidPath)
		if err != nil {
			return fmt.Errorf("无法连接守护进程: %w", err)
		}
		var info map[string]int
		json.Unmarshal(pidData, &info)
		if pid, ok := info["pid"]; ok {
			proc, err := os.FindProcess(pid)
			if err == nil {
				proc.Signal(syscall.SIGTERM)
				os.Remove(socketPath)
				os.Remove(pidPath)
				return nil
			}
		}
		return fmt.Errorf("无法连接守护进程: %w", err)
	}
	defer conn.Close()

	// 发送 shutdown 命令
	conn.Write([]byte("shutdown"))
	return nil
}

// IsRunning 检查守护进程是否运行
func IsRunning(socketPath string) bool {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Running 检查是否在运行
func Running() bool {
	return IsRunning(filepath.Join(config.Dir(), socketName))
}

func (d *Daemon) handleIPC(conn net.Conn) {
	defer conn.Close()

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		return
	}

	cmd := string(buf[:n])
	switch cmd {
	case "shutdown":
		d.engine.Stop()
		syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	case "status":
		status := fmt.Sprintf("运行中 | 模式: %s", d.engine.Mode())
		conn.Write([]byte(status))
	case "stats":
			tx, rx := d.engine.Stats()
			stats := fmt.Sprintf("上行: %s | 下行: %s",
				core.FormatBytes(tx), core.FormatBytes(rx))
			conn.Write([]byte(stats))
	case "reload":
		newCfg, err := config.Load(d.cfgPath())
		if err != nil {
			conn.Write([]byte(fmt.Sprintf("重载失败: %v", err)))
			return
		}
		if err := d.engine.Reload(newCfg); err != nil {
			conn.Write([]byte(fmt.Sprintf("重载失败: %v", err)))
			return
		}
		d.cfg = newCfg
		conn.Write([]byte("配置已重载"))
	}
}

func (d *Daemon) cfgPath() string {
	return config.DefaultPath()
}
