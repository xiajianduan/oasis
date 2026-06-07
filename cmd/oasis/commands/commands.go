package commands

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/oasis/oasis/internal/assets"
	"github.com/oasis/oasis/internal/config"
	"github.com/oasis/oasis/internal/core"
	"github.com/oasis/oasis/internal/daemon"
	"github.com/oasis/oasis/internal/subscribe"
	"github.com/oasis/oasis/internal/systemproxy"
	"github.com/oasis/oasis/internal/tun"
	"github.com/spf13/cobra"
)

var cfgPath string
var version = "0.1.0"

func Execute() error {
	return rootCmd.Execute()
}

var rootCmd = &cobra.Command{
	Use:   "oasis",
	Short: "Oasis - 轻量代理工具",
	Long:  `Oasis 是一个用配置文件驱动的代理 CLI 工具，支持 SOCKS5/TUN 模式。`,
	CompletionOptions: cobra.CompletionOptions{
		DisableDefaultCmd: true,
	},
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfgPath, "config", "c", config.DefaultPath(), "配置文件路径")
}

func loadConfig() *config.Config {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	return cfg
}

// ========== proxy (top level) ==========

var startCmd = &cobra.Command{
	Use:     "start",
	Aliases: []string{"up"},
	Short:   "启动代理",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := loadConfig()
		foreground, _ := cmd.Flags().GetBool("foreground")

		if foreground {
			return runForeground(cfg)
		}

		d, err := daemon.New(cfg)
		if err != nil {
			return err
		}
		fmt.Println("oasis 启动中...")
		return d.Start()
	},
}

func runForeground(cfg *config.Config) error {
	engine, err := core.New(cfg)
	if err != nil {
		return fmt.Errorf("初始化引擎失败: %w", err)
	}
	if err := engine.Start(); err != nil {
		return fmt.Errorf("启动引擎失败: %w", err)
	}
	fmt.Println("oasis 前台运行中 (Ctrl+C 停止)")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	fmt.Println("\noasis 正在停止...")
	engine.Stop()
	return nil
}

var stopCmd = &cobra.Command{
	Use:     "stop",
	Aliases: []string{"down"},
	Short:   "停止代理",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := daemon.Stop(); err != nil {
			return err
		}
		cleanupTUN()
		fmt.Println("oasis 已停止")
		return nil
	},
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "查看运行状态",
	RunE: func(cmd *cobra.Command, args []string) error {
		useJSON, _ := cmd.Flags().GetBool("json")

		if !daemon.Running() {
			if useJSON {
				return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "stopped"})
			}
			fmt.Println("状态: 未运行")
			return nil
		}

		socketPath := filepath.Join(config.Dir(), "oasis.sock")
		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			if useJSON {
				return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "message": err.Error()})
			}
			fmt.Println("状态: 异常 (无法连接守护进程)")
			return nil
		}
		defer conn.Close()

		conn.Write([]byte("status"))
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		msg := string(buf[:n])

		if useJSON {
			return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "running", "detail": msg})
		}
		fmt.Printf("状态: %s\n", msg)
		return nil
	},
}

var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "重启代理",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("oasis 重启中...")
		daemon.Stop()
		cleanupTUN()
		time.Sleep(500 * time.Millisecond)

		cfg := loadConfig()
		d, err := daemon.New(cfg)
		if err != nil {
			return err
		}
		return d.Start()
	},
}

// ========== node group ==========

var nodeCmd = &cobra.Command{
	Use:   "node",
	Short: "节点管理 (list/use/ping)",
}

var nodeListCmd = &cobra.Command{
	Use:   "list",
	Short: "列出所有节点",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := loadConfig()
		useJSON, _ := cmd.Flags().GetBool("json")

		type nodeItem struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Server   string `json:"server"`
			Port     int    `json:"port"`
			Selected bool   `json:"selected"`
		}
		var items []nodeItem
		for _, n := range cfg.Upstream.Nodes {
			items = append(items, nodeItem{
				Name: n.Name, Type: n.Type, Server: n.Server, Port: n.Port,
				Selected: n.Name == cfg.Upstream.Selected,
			})
		}

		if useJSON {
			return json.NewEncoder(os.Stdout).Encode(items)
		}

		fmt.Println("可用节点:")
		for _, n := range items {
			marker := " "
			if n.Selected {
				marker = "*"
			}
			fmt.Printf("  %s %s (%s://%s:%d)\n", marker, n.Name, n.Type, n.Server, n.Port)
		}
		if len(cfg.Upstream.Subscriptions) > 0 {
			fmt.Println("\n订阅源:")
			for _, s := range cfg.Upstream.Subscriptions {
				fmt.Printf("  - %s (%s)\n", s.Name, s.URL)
			}
		}
		return nil
	},
}

var nodeUseCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "切换节点",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		cfg := loadConfig()

		found := false
		for _, node := range cfg.Upstream.Nodes {
			if node.Name == name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("未找到节点: %s", name)
		}

		cfg.Upstream.Selected = name
		if err := cfg.Save(config.DefaultPath()); err != nil {
			return err
		}
		fmt.Printf("已切换到: %s\n", name)
		if daemon.Running() {
			fmt.Println("运行中，请执行 'oasis config reload' 使切换生效")
		}
		return nil
	},
}

var nodePingCmd = &cobra.Command{
	Use:   "ping <name>",
	Short: "测试节点延迟",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		cfg := loadConfig()

		node, err := cfg.GetNode(name)
		if err != nil {
			return err
		}

		addr := fmt.Sprintf("%s:%d", node.Server, node.Port)
		fmt.Printf("测试 %s (%s)...\n", name, addr)

		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			return fmt.Errorf("连接失败: %w", err)
		}
		conn.Close()
		elapsed := time.Since(start)
		fmt.Printf("延迟: %dms\n", elapsed.Milliseconds())
		return nil
	},
}

// ========== config group ==========

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "配置管理 (init/edit/show/reload)",
}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "生成配置文件",
	RunE: func(cmd *cobra.Command, args []string) error {
		path := config.DefaultPath()
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("配置文件已存在: %s", path)
		}
		yes, _ := cmd.Flags().GetBool("yes")
		cfg := config.DefaultConfig()
		if !yes {
			var err error
			cfg, err = runConfigWizard()
			if err != nil {
				return err
			}
		}
		if err := cfg.Save(path); err != nil {
			return err
		}
		fmt.Printf("配置已生成: %s\n", path)
		return nil
	},
}

func runConfigWizard() (*config.Config, error) {
	reader := bufio.NewReader(os.Stdin)
	cfg := config.DefaultConfig()

	fmt.Println("Oasis 配置向导")
	fmt.Println("推荐：开启系统代理，关闭 TUN。TUN 仅用于不支持系统代理的软件。")

	name := askString(reader, "节点名称", "Termux-Net")
	server := askString(reader, "SOCKS5 地址", "192.168.50.126")
	port, err := askInt(reader, "SOCKS5 端口", 10808)
	if err != nil {
		return nil, err
	}
	systemProxy := askBool(reader, "启用 macOS 系统代理", true)
	tunEnabled := askBool(reader, "启用 TUN 模式", false)

	cfg.Mode = "global"
	if tunEnabled {
		cfg.Mode = askChoice(reader, "代理模式", "global", []string{"global", "rule"})
	}
	cfg.TUN.Enabled = tunEnabled
	cfg.SystemProxy.Enabled = systemProxy
	cfg.Upstream.Nodes = []config.NodeConfig{
		{
			Name:   name,
			Type:   "socks5",
			Server: server,
			Port:   port,
		},
	}
	cfg.Upstream.Selected = name
	return cfg, nil
}

func askString(reader *bufio.Reader, label string, def string) string {
	fmt.Printf("%s [%s]: ", label, def)
	text, _ := reader.ReadString('\n')
	text = strings.TrimSpace(text)
	if text == "" {
		return def
	}
	return text
}

func askInt(reader *bufio.Reader, label string, def int) (int, error) {
	raw := askString(reader, label, fmt.Sprintf("%d", def))
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 || value > 65535 {
		return 0, fmt.Errorf("%s 无效: %s", label, raw)
	}
	return value, nil
}

func askBool(reader *bufio.Reader, label string, def bool) bool {
	defText := "Y/n"
	if !def {
		defText = "y/N"
	}
	fmt.Printf("%s [%s]: ", label, defText)
	text, _ := reader.ReadString('\n')
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return def
	}
	return text == "y" || text == "yes" || text == "是" || text == "true" || text == "1"
}

func askChoice(reader *bufio.Reader, label string, def string, allowed []string) string {
	fmt.Printf("%s [%s] (%s): ", label, def, strings.Join(allowed, "/"))
	text, _ := reader.ReadString('\n')
	text = strings.TrimSpace(text)
	if text == "" {
		return def
	}
	for _, item := range allowed {
		if text == item {
			return text
		}
	}
	return def
}

var configEditCmd = &cobra.Command{
	Use:   "edit",
	Short: "编辑配置文件",
	RunE: func(cmd *cobra.Command, args []string) error {
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		return runCommand(editor, config.DefaultPath())
	},
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "显示当前配置",
	RunE: func(cmd *cobra.Command, args []string) error {
		useJSON, _ := cmd.Flags().GetBool("json")
		if useJSON {
			cfg := loadConfig()
			return json.NewEncoder(os.Stdout).Encode(cfg)
		}
		data, err := os.ReadFile(config.DefaultPath())
		if err != nil {
			return fmt.Errorf("读取配置失败: %w", err)
		}
		fmt.Print(string(data))
		return nil
	},
}

var configReloadCmd = &cobra.Command{
	Use:   "reload",
	Short: "热重载配置（不重启代理）",
	RunE: func(cmd *cobra.Command, args []string) error {
		if !daemon.Running() {
			return fmt.Errorf("oasis 未在运行")
		}
		socketPath := filepath.Join(config.Dir(), "oasis.sock")
		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			return fmt.Errorf("连接守护进程失败: %w", err)
		}
		defer conn.Close()

		conn.Write([]byte("reload"))
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		fmt.Println(string(buf[:n]))
		return nil
	},
}

// ========== sub group ==========

var subCmd = &cobra.Command{
	Use:   "sub",
	Short: "订阅管理 (update/status)",
}

var subUpdateCmd = &cobra.Command{
	Use:   "update",
	Short: "手动更新所有订阅",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := loadConfig()
		if len(cfg.Upstream.Subscriptions) == 0 {
			return fmt.Errorf("未配置订阅源")
		}

		existing := make(map[string]bool)
		for _, n := range cfg.Upstream.Nodes {
			existing[n.Name] = true
		}
		totalAdded := 0

		for _, sub := range cfg.Upstream.Subscriptions {
			fmt.Printf("正在更新订阅 [%s]: %s ...\n", sub.Name, sub.URL)
			nodes, err := subscribe.FetchAndParse(sub.URL)
			if err != nil {
				fmt.Printf("  ✗ 更新失败: %v\n", err)
				continue
			}
			added := 0
			for _, n := range nodes {
				if existing[n.Name] {
					continue
				}
				cfg.Upstream.Nodes = append(cfg.Upstream.Nodes, n)
				existing[n.Name] = true
				added++
			}
			totalAdded += added
			fmt.Printf("  ✓ 新增 %d 个节点\n", added)
		}

		if err := cfg.Save(config.DefaultPath()); err != nil {
			return err
		}

		fmt.Printf("订阅更新完成: 新增 %d 个节点，共 %d 个节点\n", totalAdded, len(cfg.Upstream.Nodes))
		if totalAdded > 0 {
			fmt.Println("使用 'oasis node list' 查看，'oasis node use <name>' 切换")
		}
		return nil
	},
}

var subStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "查看订阅信息",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := loadConfig()
		if len(cfg.Upstream.Subscriptions) == 0 {
			fmt.Println("未配置订阅源")
			return nil
		}
		fmt.Println("订阅源:")
		for _, s := range cfg.Upstream.Subscriptions {
			fmt.Printf("  - %s (%s)\n", s.Name, s.URL)
		}
		return nil
	},
}

// ========== mode ==========

var modeCmd = &cobra.Command{
	Use:   "mode [global|rule|direct]",
	Short: "查看或切换模式 (global / rule / direct)",
	Long: `切换代理模式:
  global  — 全局模式，所有流量走代理
  rule    — 规则模式，按路由规则匹配
  direct  — 直连模式，不走代理

不带参数时查看当前模式。`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := loadConfig()
		if len(args) == 0 {
			fmt.Printf("当前模式: %s\n", cfg.Mode)
			return nil
		}
		mode := args[0]
		switch mode {
		case "global", "rule", "direct":
		default:
			return fmt.Errorf("无效模式: %s (可用: global / rule / direct)", mode)
		}

		cfg.Mode = mode

		if err := cfg.Save(config.DefaultPath()); err != nil {
			return err
		}
		fmt.Printf("已切换到 %s 模式\n", mode)
		if daemon.Running() {
			sendReload()
		}
		return nil
	},
}

// ========== tun group ==========

var tunCmd = &cobra.Command{
	Use:   "tun",
	Short: "TUN 模式控制 (on/off/stats)",
}

var tunOnCmd = &cobra.Command{
	Use:   "on",
	Short: "开启 TUN 模式",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := loadConfig()
		cfg.TUN.Enabled = true
		if err := cfg.Save(config.DefaultPath()); err != nil {
			return err
		}
		fmt.Println("TUN 模式已开启")
		if daemon.Running() {
			fmt.Println("请执行 'oasis config reload' 使生效")
		}
		return nil
	},
}

var tunOffCmd = &cobra.Command{
	Use:   "off",
	Short: "关闭 TUN 模式",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := loadConfig()
		cfg.TUN.Enabled = false
		if err := cfg.Save(config.DefaultPath()); err != nil {
			return err
		}
		fmt.Println("TUN 模式已关闭")
		if daemon.Running() {
			fmt.Println("请执行 'oasis config reload' 使生效")
		}
		return nil
	},
}

var tunStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "显示流量统计",
	RunE: func(cmd *cobra.Command, args []string) error {
		if !daemon.Running() {
			return fmt.Errorf("oasis 未在运行")
		}
		socketPath := filepath.Join(config.Dir(), "oasis.sock")
		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			return fmt.Errorf("连接守护进程失败: %w", err)
		}
		defer conn.Close()

		conn.Write([]byte("stats"))
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		fmt.Println(string(buf[:n]))
		return nil
	},
}

// ========== system proxy group ==========

var systemProxyCmd = &cobra.Command{
	Use:   "system-proxy",
	Short: "系统代理控制 (on/off/status)",
}

var systemProxyOnCmd = &cobra.Command{
	Use:   "on",
	Short: "开启 macOS 系统 SOCKS 代理",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := loadConfig()
		cfg.SystemProxy.Enabled = true
		if err := cfg.Save(config.DefaultPath()); err != nil {
			return err
		}
		if cfg.Mode != "rule" {
			if err := enableConfiguredSystemProxy(cfg); err != nil {
				return err
			}
		}
		fmt.Println("系统代理已开启")
		if daemon.Running() {
			sendReload()
		}
		return nil
	},
}

var systemProxyOffCmd = &cobra.Command{
	Use:   "off",
	Short: "关闭 macOS 系统代理",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := loadConfig()
		cfg.SystemProxy.Enabled = false
		if err := cfg.Save(config.DefaultPath()); err != nil {
			return err
		}
		systemproxy.Disable()
		fmt.Println("系统代理已关闭")
		if daemon.Running() {
			sendReload()
		}
		return nil
	},
}

var systemProxyStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "查看系统代理配置状态",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := loadConfig()
		state := "关闭"
		if cfg.SystemProxy.Enabled {
			state = "开启"
		}
		fmt.Printf("Oasis 系统代理配置: %s\n", state)
		return nil
	},
}

// ========== logs ==========

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "查看日志",
	RunE: func(cmd *cobra.Command, args []string) error {
		follow, _ := cmd.Flags().GetBool("follow")
		n, _ := cmd.Flags().GetInt("n")
		errorOnly, _ := cmd.Flags().GetBool("error")

		logPath := filepath.Join(config.Dir(), "oasis.log")

		if !follow {
			data, err := os.ReadFile(logPath)
			if err != nil {
				return fmt.Errorf("无法读取日志: %w", err)
			}

			lines := strings.Split(string(data), "\n")

			// 过滤错误
			if errorOnly {
				var filtered []string
				for _, l := range lines {
					if strings.Contains(l, "error") || strings.Contains(l, "Error") || strings.Contains(l, "ERROR") || strings.Contains(l, "fail") || strings.Contains(l, "Fail") {
						filtered = append(filtered, l)
					}
				}
				lines = filtered
			}

			// 取最后 N 行
			if n > 0 && n < len(lines) {
				lines = lines[len(lines)-n:]
			}

			fmt.Print(strings.Join(lines, "\n"))
			if len(lines) > 0 && lines[len(lines)-1] != "" {
				fmt.Println()
			}
			return nil
		}

		// follow 模式（tail -f）
		f, err := os.Open(logPath)
		if err != nil {
			return fmt.Errorf("无法打开日志: %w", err)
		}
		defer f.Close()

		f.Seek(0, 2)
		buf := make([]byte, 4096)
		for {
			_, err := f.Read(buf)
			if err != nil {
				time.Sleep(500 * time.Millisecond)
			}
		}
	},
}

// ========== doctor ==========

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "系统自检",
	RunE: func(cmd *cobra.Command, args []string) error {
		useJSON, _ := cmd.Flags().GetBool("json")
		results := doDoctor()

		if useJSON {
			return json.NewEncoder(os.Stdout).Encode(results)
		}

		fmt.Println("=== Oasis 系统自检 ===")
		for _, r := range results {
			icon := "✓"
			if r.Status != "ok" {
				icon = "✗"
			}
			fmt.Printf("  %s %s: %s\n", icon, r.Name, r.Status)
			if r.Detail != "" {
				fmt.Printf("    %s\n", r.Detail)
			}
		}
		return nil
	},
}

type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func doDoctor() []checkResult {
	var results []checkResult

	// 检查配置文件
	configPath := config.DefaultPath()
	if _, err := os.Stat(configPath); err != nil {
		results = append(results, checkResult{Name: "配置文件", Status: "未找到", Detail: configPath})
	} else if _, err := config.Load(configPath); err != nil {
		results = append(results, checkResult{Name: "配置文件", Status: "解析失败", Detail: err.Error()})
	} else {
		results = append(results, checkResult{Name: "配置文件", Status: "ok"})
	}

	// 检查 tun2socks
	if _, err := assets.Tun2socksPath(); err != nil {
		results = append(results, checkResult{Name: "tun2socks", Status: "不可用", Detail: err.Error()})
	} else {
		results = append(results, checkResult{Name: "tun2socks", Status: "ok"})
	}

	// 检查守护进程
	if daemon.Running() {
		results = append(results, checkResult{Name: "守护进程", Status: "运行中"})
	} else {
		results = append(results, checkResult{Name: "守护进程", Status: "未运行"})
	}

	// 检查环境
	results = append(results, checkResult{Name: "系统", Status: fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)})

	// 上游连通性检测
	cfg, err := config.Load(config.DefaultPath())
	if err == nil {
		node, err := cfg.GetSelectedNode()
		if err == nil {
			upstreamAddr := fmt.Sprintf("%s:%d", node.Server, node.Port)
			conn, err := net.DialTimeout("tcp", upstreamAddr, 5*time.Second)
			if err != nil {
				results = append(results, checkResult{
					Name: "上游节点", Status: "TCP 不可达",
					Detail: fmt.Sprintf("%s (%s): %v", node.Name, upstreamAddr, err),
				})
			} else {
				buf := []byte{5, 1, 0}
				conn.Write(buf)
				resp := make([]byte, 2)
				conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				_, err := conn.Read(resp)
				conn.Close()
				if err != nil || resp[0] != 5 {
					results = append(results, checkResult{
						Name: "上游节点", Status: "SOCKS5 握手失败",
						Detail: fmt.Sprintf("%s (%s): 不是有效的 SOCKS5 服务", node.Name, upstreamAddr),
					})
				} else {
					results = append(results, checkResult{
						Name: "上游节点", Status: "正常",
						Detail: fmt.Sprintf("%s (%s) 延迟 < 5s", node.Name, upstreamAddr),
					})
				}
			}
		} else {
			results = append(results, checkResult{Name: "上游节点", Status: "未配置", Detail: err.Error()})
		}
	}

	return results
}

// ========== info ==========

var infoCmd = &cobra.Command{
	Use:   "info",
	Short: "显示版本和环境信息",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Printf("Oasis v%s\n", version)
		fmt.Printf("Go: %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
		fmt.Printf("配置: %s\n", config.DefaultPath())
		fmt.Printf("数据: %s\n", config.Dir())
		return nil
	},
}

// ========== clean ==========

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "清理临时文件",
	RunE: func(cmd *cobra.Command, args []string) error {
		force, _ := cmd.Flags().GetBool("force")

		if daemon.Running() && !force {
			return fmt.Errorf("oasis 正在运行，请先执行 'oasis down' 停止，或使用 --force 强制清理")
		}

		oasisDir := config.Dir()
		removed := 0
		cleanupTUN()

		for _, name := range []string{"oasis.sock", "oasis.pid", "oasis.log"} {
			p := filepath.Join(oasisDir, name)
			if err := os.Remove(p); err == nil {
				fmt.Printf("已删除: %s\n", p)
				removed++
			}
		}

		if removed == 0 {
			fmt.Println("没有需要清理的文件")
		} else {
			fmt.Printf("清理完成，共删除 %d 个文件\n", removed)
		}
		return nil
	},
}

// ========== version ==========

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "显示版本号",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("oasis v%s\n", version)
	},
}

// ========== log setup ==========

func setupLog() {
	oasisDir := config.Dir()
	os.MkdirAll(oasisDir, 0755)
	logPath := filepath.Join(oasisDir, "oasis.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
}

// ========== init ==========

func init() {
	// 代理控制 (top level)
	startCmd.Flags().BoolP("foreground", "F", false, "前台运行（调试用）")
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(stopCmd)

	statusCmd.Flags().Bool("json", false, "JSON 格式输出")
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(restartCmd)

	// 模式切换
	rootCmd.AddCommand(modeCmd)
	// 节点管理
	nodeListCmd.Flags().Bool("json", false, "JSON 格式输出")
	nodeCmd.AddCommand(nodeListCmd, nodeUseCmd, nodePingCmd)
	rootCmd.AddCommand(nodeCmd)

	// 配置管理
	configInitCmd.Flags().Bool("yes", false, "跳过向导，生成默认配置")
	configShowCmd.Flags().Bool("json", false, "JSON 格式输出")
	configCmd.AddCommand(configInitCmd, configEditCmd, configShowCmd, configReloadCmd)
	rootCmd.AddCommand(configCmd)

	// TUN
	tunCmd.AddCommand(tunOnCmd, tunOffCmd, tunStatsCmd)
	rootCmd.AddCommand(tunCmd)

	// 系统代理
	systemProxyCmd.AddCommand(systemProxyOnCmd, systemProxyOffCmd, systemProxyStatusCmd)
	rootCmd.AddCommand(systemProxyCmd)

	// 订阅
	subCmd.AddCommand(subUpdateCmd, subStatusCmd)
	rootCmd.AddCommand(subCmd)

	// 日志与调试
	logsCmd.Flags().BoolP("follow", "f", false, "跟踪日志")
	logsCmd.Flags().IntP("n", "n", 0, "显示最近 N 行")
	logsCmd.Flags().Bool("error", false, "只看错误日志")
	rootCmd.AddCommand(logsCmd)

	doctorCmd.Flags().Bool("json", false, "JSON 格式输出")
	rootCmd.AddCommand(doctorCmd)

	rootCmd.AddCommand(infoCmd)

	// 隐藏 help 命令
	rootCmd.SetHelpCommand(&cobra.Command{Hidden: true})

	// 其他
	cleanCmd.Flags().Bool("force", false, "强制清理，不询问")
	rootCmd.AddCommand(cleanCmd)
	rootCmd.AddCommand(versionCmd)
}

// ========== helpers ==========

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func sendReload() {
	socketPath := filepath.Join(config.Dir(), "oasis.sock")
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Println("(无法通知守护进程，请执行 'oasis restart')")
		return
	}
	defer conn.Close()
	conn.Write([]byte("reload"))
	buf := make([]byte, 1024)
	n, _ := conn.Read(buf)
	fmt.Println(string(buf[:n]))
}

func cleanupTUN() {
	systemproxy.Disable()
	tun.DisableDNSRedirect()
	tun.RestoreDNS()
	exec.Command("sudo", "-n", "pkill", "-x", "tun2socks").Run()
	sudoRoute("route", "delete", "-net", "0.0.0.0", "-netmask", "128.0.0.0")
	sudoRoute("route", "delete", "-net", "128.0.0.0", "-netmask", "128.0.0.0")
	cleanupUpstreamRoute()
}

func cleanupUpstreamRoute() {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return
	}
	node, err := cfg.GetSelectedNode()
	if err != nil {
		return
	}
	if ip := net.ParseIP(node.Server); ip != nil {
		sudoRoute("route", "-n", "delete", "-host", ip.String())
	}
}

func enableConfiguredSystemProxy(cfg *config.Config) error {
	node, err := cfg.GetSelectedNode()
	if err != nil {
		return err
	}
	if node.Type != "socks5" {
		return fmt.Errorf("系统代理仅能直接指向 SOCKS5 节点，当前节点类型: %s；请切换到 rule 模式使用本地 SOCKS 入口", node.Type)
	}
	return systemproxy.Enable(node.Server, node.Port, "", 0)
}

func sudoRoute(args ...string) {
	if os.Geteuid() != 0 {
		exec.Command("sudo", append([]string{"-n"}, args...)...).Run()
	} else {
		exec.Command(args[0], args[1:]...).Run()
	}
}
