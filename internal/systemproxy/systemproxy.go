package systemproxy

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

// activeService 检测当前活跃的网络服务名（Wi-Fi / Ethernet 等）。
func activeService() string {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return "Wi-Fi"
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "An asterisk") {
			continue
		}
		switch s {
		case "Bluetooth PAN", "Thunderbolt Bridge", "Thunderbolt 1", "Thunderbolt 2", "iPhone USB", "iPad USB":
			continue
		}
		info, _ := exec.Command("networksetup", "-getinfo", s).Output()
		if strings.Contains(string(info), "IP address:") && !strings.Contains(string(info), "IP address: none") {
			return s
		}
	}
	return "Wi-Fi"
}

func networksetup(args ...string) error {
	if os.Geteuid() == 0 {
		out, err := exec.Command("/usr/sbin/networksetup", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("networksetup 失败: %v, output: %s", err, string(out))
		}
		return nil
	}
	if exec.Command("sudo", append([]string{"-n", "/usr/sbin/networksetup"}, args...)...).Run() == nil {
		return nil
	}
	return fmt.Errorf("sudo -n /usr/sbin/networksetup %s 失败，请先运行 make install 配置 sudoers", strings.Join(args, " "))
}

// Enable 设置当前活跃网络服务的系统 SOCKS / HTTP / HTTPS 代理。
func Enable(socksHost string, socksPort int, httpHost string, httpPort int) error {
	service := activeService()
	if err := networksetup("-setsocksfirewallproxy", service, socksHost, fmt.Sprintf("%d", socksPort)); err != nil {
		return err
	}
	if err := networksetup("-setsocksfirewallproxystate", service, "on"); err != nil {
		return err
	}
	if httpHost != "" && httpPort > 0 {
		if err := networksetup("-setwebproxy", service, httpHost, fmt.Sprintf("%d", httpPort)); err != nil {
			return err
		}
		if err := networksetup("-setwebproxystate", service, "on"); err != nil {
			return err
		}
		if err := networksetup("-setsecurewebproxy", service, httpHost, fmt.Sprintf("%d", httpPort)); err != nil {
			return err
		}
		if err := networksetup("-setsecurewebproxystate", service, "on"); err != nil {
			return err
		}
		log.Printf("[system-proxy] HTTP/HTTPS 已开启: %s:%d (服务: %s)", httpHost, httpPort, service)
	} else {
		_ = networksetup("-setwebproxystate", service, "off")
		_ = networksetup("-setsecurewebproxystate", service, "off")
	}
	log.Printf("[system-proxy] SOCKS 已开启: %s:%d (服务: %s)", socksHost, socksPort, service)
	return nil
}

// Disable 关闭当前活跃网络服务的系统 SOCKS / HTTP / HTTPS 代理。
func Disable() {
	service := activeService()
	for _, args := range [][]string{
		{"-setsocksfirewallproxystate", service, "off"},
		{"-setwebproxystate", service, "off"},
		{"-setsecurewebproxystate", service, "off"},
	} {
		if err := networksetup(args...); err != nil {
			log.Printf("[system-proxy] 关闭失败: %v", err)
		}
	}
	log.Printf("[system-proxy] 系统代理已关闭 (服务: %s)", service)
}
