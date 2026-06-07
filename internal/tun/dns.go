package tun

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

const dnsPfAnchor = "com.oasis.dns"

func pfctlSudo(args ...string) error {
	if os.Geteuid() == 0 {
		return exec.Command("/sbin/pfctl", args...).Run()
	}
	if exec.Command("sudo", append([]string{"-n", "/sbin/pfctl"}, args...)...).Run() == nil {
		return nil
	}
	// sudo -n 失败，弹窗提权
	script := fmt.Sprintf("/sbin/pfctl %s", strings.Join(args, " "))
	cmd := exec.Command("osascript", "-e",
		fmt.Sprintf(`do shell script "%s" with administrator privileges`, script))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pfctl 失败: %v, output: %s", err, string(out))
	}
	return nil
}

// EnableDNSRedirect 用 pf 将 127.0.0.1:53 → 127.0.0.1:targetPort
func EnableDNSRedirect(targetPort string) error {
	pfctlSudo("-a", dnsPfAnchor, "-F", "all")

	rule := fmt.Sprintf("rdr pass on lo0 proto udp from any to 127.0.0.1 port 53 -> 127.0.0.1 port %s\n", targetPort)
	tmpFile, err := os.CreateTemp("", "oasis-pf-*.conf")
	if err != nil {
		return fmt.Errorf("创建临时 pf 规则文件失败: %w", err)
	}
	tmpFile.WriteString(rule)
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	if err := pfctlSudo("-a", dnsPfAnchor, "-f", tmpFile.Name()); err != nil {
		return err
	}

	log.Printf("[tun] DNS 重定向已启用 (53 → %s)", targetPort)
	return nil
}

// DisableDNSRedirect 移除 pf DNS 重定向规则
func DisableDNSRedirect() {
	if err := pfctlSudo("-a", dnsPfAnchor, "-F", "all"); err != nil {
		log.Printf("[tun] 关闭 DNS 重定向失败: %v", err)
		return
	}
	log.Printf("[tun] DNS 重定向已关闭")
}
