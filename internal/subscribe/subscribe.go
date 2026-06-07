package subscribe

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oasis/oasis/internal/config"
	"gopkg.in/yaml.v3"
)

// ClashConfig clash 订阅格式
type ClashConfig struct {
	Proxies []ClashProxy `yaml:"proxies"`
}

// ClashProxy clash 代理节点
type ClashProxy struct {
	Name     string `yaml:"name"`
	Type     string `yaml:"type"`
	Server   string `yaml:"server"`
	Port     int    `yaml:"port"`
	Password string `yaml:"password"`
	UUID     string `yaml:"uuid"`
	Cipher   string `yaml:"cipher"`
	Username string `yaml:"username"`
}

type UnsupportedTypesError struct {
	Types map[string]int
}

func (e UnsupportedTypesError) Error() string {
	if len(e.Types) == 0 {
		return "订阅中没有节点"
	}
	var parts []string
	for typ, count := range e.Types {
		parts = append(parts, fmt.Sprintf("%s:%d", typ, count))
	}
	return fmt.Sprintf("订阅中没有 Oasis 当前支持的节点类型，发现类型: %s", strings.Join(parts, ", "))
}

// FetchAndParse 获取并解析订阅
func FetchAndParse(url string) ([]config.NodeConfig, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return fetchAndParseWithClient(client, url)
}

// FetchAndParseViaNode 通过当前 SOCKS5 节点获取并解析订阅。
func FetchAndParseViaNode(rawURL string, node *config.NodeConfig) ([]config.NodeConfig, error) {
	if node == nil || node.Type != "socks5" {
		return FetchAndParse(rawURL)
	}
	proxyURL, err := url.Parse(fmt.Sprintf("socks5://%s:%d", node.Server, node.Port))
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}
	return fetchAndParseWithClient(client, rawURL)
}

func fetchAndParseWithClient(client *http.Client, url string) ([]config.NodeConfig, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "clash.meta")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("获取订阅失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("获取订阅失败: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取订阅内容失败: %w", err)
	}

	content := string(body)

	// 尝试 base64 解码
	if decoded, err := base64.StdEncoding.DecodeString(content); err == nil {
		content = string(decoded)
	} else if decoded, err := base64.RawStdEncoding.DecodeString(content); err == nil {
		content = string(decoded)
	}

	// 尝试 clash yaml 格式
	if strings.Contains(content, "proxies:") {
		return parseClashYAML(content)
	}

	// 尝试 v2ray 分享链接格式
	if strings.Contains(content, "://") {
		return parseShareLinks(content)
	}

	return nil, fmt.Errorf("不支持的订阅格式")
}

func parseClashYAML(content string) ([]config.NodeConfig, error) {
	var clash ClashConfig
	if err := yaml.Unmarshal([]byte(content), &clash); err != nil {
		return nil, fmt.Errorf("解析 clash 配置失败: %w", err)
	}

	var nodes []config.NodeConfig
	unsupported := make(map[string]int)
	for _, p := range clash.Proxies {
		node := config.NodeConfig{
			Name:     p.Name,
			Server:   p.Server,
			Port:     p.Port,
			Password: p.Password,
			Username: p.Username,
		}

		switch p.Type {
		case "ss":
			node.Type = "shadowsocks"
			node.Method = p.Cipher
		case "vmess":
			unsupported[p.Type]++
			continue
		case "trojan":
			unsupported[p.Type]++
			continue
		case "socks5":
			node.Type = "socks5"
		default:
			unsupported[p.Type]++
			continue
		}

		nodes = append(nodes, node)
	}

	if len(nodes) == 0 && len(clash.Proxies) > 0 {
		return nil, UnsupportedTypesError{Types: unsupported}
	}
	return nodes, nil
}

func parseShareLinks(content string) ([]config.NodeConfig, error) {
	// 分享链接格式: ss://base64(encrypt_method:password)@server:port#name
	var nodes []config.NodeConfig

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "ss://") {
			node, err := parseSSLink(line)
			if err == nil {
				nodes = append(nodes, node)
			}
		}
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("未能解析任何节点")
	}

	return nodes, nil
}

func parseSSLink(link string) (config.NodeConfig, error) {
	// ss://BASE64(method:password)@server:port#name
	// 或 ss://BASE64(method:password@server:port)#name
	link = strings.TrimPrefix(link, "ss://")

	// 分离 fragment (#name)
	name := ""
	if idx := strings.Index(link, "#"); idx != -1 {
		name = link[idx+1:]
		link = link[:idx]
	}

	// 分离 @ 前后
	parts := strings.SplitN(link, "@", 2)
	if len(parts) != 2 {
		return config.NodeConfig{}, fmt.Errorf("SS 链接格式错误")
	}

	// 解码 method:password
	userInfo, err := base64.RawStdEncoding.DecodeString(parts[0])
	if err != nil {
		userInfo, err = base64.StdEncoding.DecodeString(parts[0])
		if err != nil {
			return config.NodeConfig{}, fmt.Errorf("解码 SS 用户信息失败")
		}
	}

	userParts := strings.SplitN(string(userInfo), ":", 2)
	if len(userParts) != 2 {
		return config.NodeConfig{}, fmt.Errorf("SS 用户信息格式错误")
	}

	method := userParts[0]
	password := userParts[1]

	// 解析 server:port
	host, portStr, err := splitHostPort(parts[1])
	if err != nil {
		return config.NodeConfig{}, err
	}
	port := 0
	fmt.Sscanf(portStr, "%d", &port)

	if name == "" {
		name = host
	}

	return config.NodeConfig{
		Name:     name,
		Type:     "shadowsocks",
		Server:   host,
		Port:     port,
		Method:   method,
		Password: password,
	}, nil
}

func splitHostPort(s string) (host, port string, err error) {
	if idx := strings.LastIndex(s, ":"); idx != -1 {
		return s[:idx], s[idx+1:], nil
	}
	return "", "", fmt.Errorf("无法解析 host:port: %s", s)
}
