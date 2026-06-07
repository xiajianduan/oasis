package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Dir 返回数据目录 ~/.config/oasis/
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "oasis")
}

// DefaultPath 默认配置文件路径
func DefaultPath() string {
	return filepath.Join(Dir(), "config.yaml")
}

// Config 顶层配置
type Config struct {
	Mode        string            `yaml:"mode"` // global / rule / direct
	TUN         TUNConfig         `yaml:"tun"`
	SystemProxy SystemProxyConfig `yaml:"system-proxy"`
	Upstream    UpstreamConfig    `yaml:"upstream"`
	Rules       []string          `yaml:"rules"`
}

// TUNConfig TUN 模式配置
type TUNConfig struct {
	Enabled bool     `yaml:"enabled"`
	Device  string   `yaml:"device"`
	MTU     int      `yaml:"mtu"`
	DNS     []string `yaml:"dns"`
	Via     string   `yaml:"via"`
}

// SystemProxyConfig macOS 系统代理配置
type SystemProxyConfig struct {
	Enabled bool `yaml:"enabled"`
}

// SubItem 单个订阅源
type SubItem struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

// UpstreamConfig 上游配置
type UpstreamConfig struct {
	Nodes         []NodeConfig `yaml:"nodes"`
	Subscriptions []SubItem    `yaml:"subscriptions"`
	Selected      string       `yaml:"selected"`
}

// NodeConfig 单个节点配置
type NodeConfig struct {
	Name   string `yaml:"name"`
	Type   string `yaml:"type"` // socks5 | shadowsocks
	Server string `yaml:"server"`
	Port   int    `yaml:"port"`
	// Shadowsocks
	Method   string `yaml:"method,omitempty"`
	Password string `yaml:"password,omitempty"`
	// SOCKS5 鉴权
	Username string `yaml:"username,omitempty"`
}

// Rule 解析后的规则
type Rule struct {
	Type   string // DOMAIN-SUFFIX | DOMAIN | IP-CIDR | MATCH
	Param  string // google.com | 192.168.0.0/16 | (空)
	Action string // DIRECT | PROXY | REJECT
}

// Load 加载配置文件
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	if !cfg.SystemProxy.Enabled {
		var legacy struct {
			SystemProxy SystemProxyConfig `yaml:"system_proxy"`
		}
		if err := yaml.Unmarshal(data, &legacy); err == nil && legacy.SystemProxy.Enabled {
			cfg.SystemProxy = legacy.SystemProxy
		}
	}

	// 设置默认值
	if cfg.Mode == "" {
		cfg.Mode = "rule"
	}
	if cfg.TUN.MTU == 0 {
		cfg.TUN.MTU = 1500
	}
	if cfg.TUN.Device == "" {
		cfg.TUN.Device = "utun"
	}
	if len(cfg.TUN.DNS) == 0 {
		cfg.TUN.DNS = []string{"8.8.8.8", "1.1.1.1"}
	}

	return cfg, nil
}

// Save 保存配置到文件
func (c *Config) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(4)
	if err := encoder.Encode(c); err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	encoder.Close()

	return os.WriteFile(path, buf.Bytes(), 0644)
}

// GetSelectedNode 获取当前选中的节点
func (c *Config) GetSelectedNode() (*NodeConfig, error) {
	for i := range c.Upstream.Nodes {
		if c.Upstream.Nodes[i].Name == c.Upstream.Selected {
			return &c.Upstream.Nodes[i], nil
		}
	}
	return nil, fmt.Errorf("未找到节点: %s", c.Upstream.Selected)
}

// GetNode 根据名称获取节点
func (c *Config) GetNode(name string) (*NodeConfig, error) {
	for i := range c.Upstream.Nodes {
		if c.Upstream.Nodes[i].Name == name {
			return &c.Upstream.Nodes[i], nil
		}
	}
	return nil, fmt.Errorf("未找到节点: %s", name)
}

// ParseRule 解析单条规则字符串
// 格式: 类型,参数,动作 (严格无空格)
func ParseRule(raw string) (*Rule, error) {
	parts := splitRule(raw)
	if len(parts) < 2 {
		return nil, fmt.Errorf("规则格式错误: %s (应为: 类型,参数,动作)", raw)
	}

	r := &Rule{Type: parts[0]}

	switch r.Type {
	case "MATCH":
		if len(parts) != 2 {
			return nil, fmt.Errorf("MATCH 规则格式: MATCH,动作")
		}
		r.Action = parts[1]
	case "DOMAIN-SUFFIX", "DOMAIN", "IP-CIDR":
		if len(parts) != 3 {
			return nil, fmt.Errorf("%s 规则格式: %s,参数,动作", r.Type, r.Type)
		}
		r.Param = parts[1]
		r.Action = parts[2]
	default:
		return nil, fmt.Errorf("未知规则类型: %s", r.Type)
	}

	return r, nil
}

// ParseRules 解析所有规则
func ParseRules(raw []string) ([]Rule, error) {
	var rules []Rule
	for _, r := range raw {
		rule, err := ParseRule(r)
		if err != nil {
			return nil, err
		}
		rules = append(rules, *rule)
	}
	return rules, nil
}

func splitRule(s string) []string {
	var parts []string
	current := ""
	for _, ch := range s {
		if ch == ',' {
			parts = append(parts, current)
			current = ""
		} else {
			current += string(ch)
		}
	}
	if current != "" || len(parts) > 0 {
		parts = append(parts, current)
	}
	return parts
}

// DefaultConfig 返回默认配置
func DefaultConfig() *Config {
	return &Config{
		Mode: "rule",
		TUN: TUNConfig{
			Enabled: false,
			Device:  "utun",
			MTU:     1500,
			DNS:     []string{"8.8.8.8", "1.1.1.1"},
		},
		SystemProxy: SystemProxyConfig{
			Enabled: false,
		},
		Upstream: UpstreamConfig{
			Nodes: []NodeConfig{
				{
					Name:   "示例节点",
					Type:   "socks5",
					Server: "192.168.1.100",
					Port:   1080,
				},
			},
			Selected: "示例节点",
		},
		Rules: []string{
			"IP-CIDR,192.168.0.0/16,DIRECT",
			"IP-CIDR,10.0.0.0/8,DIRECT",
			"IP-CIDR,127.0.0.0/8,DIRECT",
			"MATCH,PROXY",
		},
	}
}
