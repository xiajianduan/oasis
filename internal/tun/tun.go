package tun

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// Device 代表一个 TUN 设备
type Device struct {
	name string
	file *os.File
	mtu  int
	mu   sync.Mutex
}

const (
	afSystem        = 32 // AF_SYSTEM on macOS
	sysProtoControl = 2  // SYSPROTO_CONTROL
	utunControlName = "com.apple.net.utun_control"
	ctlInfoFunc     = 0xC0644E03
	utunOptIfName   = 2
)

// Open 创建/打开 TUN 设备 (macOS 通过 socket 方式)
func Open(name string, mtu int) (*Device, error) {
	// macOS 通过 AF_SYSTEM socket 连接 utun_control 来创建 utun 设备
	// 参考: https://developer.apple.com/documentation/network/utun

	fd, err := syscall.Socket(afSystem, syscall.SOCK_DGRAM, sysProtoControl)
	if err != nil {
		return nil, fmt.Errorf("创建系统 socket 失败: %w (TUN 模式需要 root 权限)", err)
	}

	// 连接到 utun control
	var ctlInfo struct {
		ctlID   uint32
		ctlName [96]byte
	}
	copy(ctlInfo.ctlName[:], utunControlName)

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(ctlInfoFunc), uintptr(unsafe.Pointer(&ctlInfo)))
	if errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("获取 utun control ID 失败: %v", errno)
	}

	// connect 到 utun_control
	var sockCtl struct {
		scLen      uint8
		scFamily   uint8
		ssSysType  uint16
		scID       uint32
		scUnit     uint32
		scReserved [5]uint32
	}
	sockCtl.scLen = uint8(unsafe.Sizeof(sockCtl))
	sockCtl.scFamily = afSystem
	sockCtl.ssSysType = sysProtoControl
	sockCtl.scID = ctlInfo.ctlID

	if name == "" || name == "utun" {
		sockCtl.scUnit = 0 // 自动分配
	} else {
		// 从名称提取编号
		var unit uint32
		fmt.Sscanf(name, "utun%d", &unit)
		sockCtl.scUnit = unit + 1
	}

	rsa, _, errno := syscall.RawSyscall(syscall.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&sockCtl)), unsafe.Sizeof(sockCtl))
	if errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("连接 utun 失败: %v (TUN 模式需要 root 权限)", errno)
	}
	_ = rsa

	// 获取分配的接口名
	var ifName [16]byte
	ifNameLen := uintptr(unsafe.Sizeof(ifName))
	_, _, errno = syscall.Syscall6(syscall.SYS_GETSOCKOPT, uintptr(fd), sysProtoControl, utunOptIfName,
		uintptr(unsafe.Pointer(&ifName)), uintptr(unsafe.Pointer(&ifNameLen)), 0)
	if errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("获取 utun 接口名失败: %v", errno)
	}

	devName := string(ifName[:ifNameLen-1]) // 去掉 null 终止符

	// 将 fd 转为 *os.File
	file := os.NewFile(uintptr(fd), "/dev/"+devName)

	dev := &Device{
		name: devName,
		file: file,
		mtu:  mtu,
	}

	return dev, nil
}

// Name 返回设备名
func (d *Device) Name() string {
	return d.name
}

// Read 读取一个 IP 包
func (d *Device) Read(packet []byte) (int, error) {
	return d.file.Read(packet)
}

// Write 写入一个 IP 包
func (d *Device) Write(packet []byte) (int, error) {
	return d.file.Write(packet)
}

// Close 关闭设备
func (d *Device) Close() error {
	return d.file.Close()
}

// Fd 返回文件描述符
func (d *Device) Fd() uintptr {
	return d.file.Fd()
}

// configureDevice 配置 TUN 设备的 IP 和路由
func ConfigureDevice(name string, ipAddr string, mtu int) error {
	// 设置 IP 地址
	output, err := exec.Command("ifconfig", name, "inet", ipAddr, ipAddr, "up").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ifconfig 配置 IP 失败: %v, output: %s", err, string(output))
	}

	// 设置 MTU
	if mtu > 0 {
		output, err = exec.Command("ifconfig", name, "mtu", fmt.Sprintf("%d", mtu)).CombinedOutput()
		if err != nil {
			log.Printf("[tun] 设置 MTU 失败: %v, output: %s", err, string(output))
		}
	}

	return nil
}

// AddRoute 添加路由规则
func AddRoute(cidr string, gateway string) error {
	// 使用 route add 或 route change
	output, err := exec.Command("route", "add", "-net", cidr, "-interface", gateway).CombinedOutput()
	if err != nil {
		// 尝试用 change
		output2, err2 := exec.Command("route", "change", "-net", cidr, "-interface", gateway).CombinedOutput()
		if err2 != nil {
			return fmt.Errorf("添加路由失败: %v (add: %s, change: %s)", err, string(output), string(output2))
		}
		log.Printf("[tun] 路由已更新: %s -> %s", cidr, gateway)
	}
	return nil
}

// activeService 检测当前活跃的网络服务名（Wi-Fi / Ethernet 等）
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

// AddDNS 将系统 DNS 设为指定服务器
func AddDNS(servers []string) error {
	service := activeService()
	if len(servers) == 0 {
		return nil
	}
	args := append([]string{"-setdnsservers", service}, servers...)
	if err := networksetup(args...); err != nil {
		return fmt.Errorf("设置 DNS 失败: %w", err)
	}
	log.Printf("[tun] 系统 DNS 设为 %v (服务: %s)", servers, service)
	return nil
}

// RestoreDNS 恢复系统 DNS 为 DHCP 自动获取
func RestoreDNS() error {
	service := activeService()
	if err := networksetup("-setdnsservers", service, "empty"); err != nil {
		return err
	}
	log.Printf("[tun] 系统 DNS 已恢复 (服务: %s)", service)
	return nil
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

// ParseIPPacket 解析 IP 包头，返回源IP、目标IP、协议、负载
func ParseIPPacket(packet []byte) (srcIP net.IP, dstIP net.IP, protocol int, payload []byte, err error) {
	if len(packet) < 20 {
		return nil, nil, 0, nil, fmt.Errorf("IP 包太短: %d bytes", len(packet))
	}

	version := packet[0] >> 4
	if version != 4 {
		return nil, nil, 0, nil, fmt.Errorf("仅支持 IPv4, 当前版本: %d", version)
	}

	ihl := int(packet[0]&0x0f) * 4
	if ihl < 20 || ihl > len(packet) {
		return nil, nil, 0, nil, fmt.Errorf("无效的 IHL: %d", ihl)
	}

	protocol = int(packet[9])
	srcIP = net.IP(packet[12:16])
	dstIP = net.IP(packet[16:20])
	payload = packet[ihl:]

	return srcIP, dstIP, protocol, payload, nil
}

// BuildTCPTarget 从 TCP payload 提取目标端口
func BuildTCPTarget(dstIP net.IP, payload []byte) string {
	if len(payload) < 2 {
		return ""
	}
	port := int(payload[0])<<8 | int(payload[1])
	return fmt.Sprintf("%s:%d", dstIP.String(), port)
}

// BuildUDPTarget 从 UDP payload 提取目标端口
func BuildUDPTarget(dstIP net.IP, payload []byte) string {
	if len(payload) < 2 {
		return ""
	}
	port := int(payload[0])<<8 | int(payload[1])
	return fmt.Sprintf("%s:%d", dstIP.String(), port)
}
