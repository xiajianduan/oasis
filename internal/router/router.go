package router

import (
	"net"
	"strings"

	"github.com/oasis/oasis/internal/config"
)

// Router 规则路由引擎
type Router struct {
	rules []config.Rule
}

// New 创建路由器
func New(rules []config.Rule) *Router {
	return &Router{rules: rules}
}

// Match 匹配目标地址，返回动作
// target 格式: "host:port" 或 "ip:port"
func (r *Router) Match(target string) string {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return "PROXY" // 解析失败默认代理
	}

	ip := net.ParseIP(host)

	for _, rule := range r.rules {
		switch rule.Type {
		case "DOMAIN":
			if host == rule.Param {
				return rule.Action
			}
		case "DOMAIN-SUFFIX":
			if strings.HasSuffix(host, rule.Param) {
				return rule.Action
			}
		case "IP-CIDR":
			if ip != nil {
				_, cidr, err := net.ParseCIDR(rule.Param)
				if err == nil && cidr.Contains(ip) {
					return rule.Action
				}
			}
		case "MATCH":
			return rule.Action
		}
	}

	// 没有匹配到任何规则，默认代理
	return "PROXY"
}
