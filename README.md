# Oasis

Oasis 是一个用 Go 编写的轻量级代理 CLI 工具，专为 macOS 设计。支持本地 SOCKS5 代理 + 可选 TUN 透明代理。

## 功能

- **SOCKS5 入站** — 本地 SOCKS5 代理服务（无需 root）
- **TUN 模式** — 基于 [tun2socks](https://github.com/xjasonlyu/tun2socks) 的全局透明代理（可选）
- **多上游支持** — SOCKS5 / Shadowsocks (AEAD) 上游节点
- **规则路由** — DOMAIN / IP-CIDR 匹配，支持 DIRECT / PROXY / REJECT
- **订阅解析** — 支持 Clash YAML 和 `ss://` 分享链接
- **守护进程** — Unix Socket IPC，后台运行

## 安装

### 前置要求

- macOS 系统
- Go 1.26+
- curl / unzip

### 编译

```bash
git clone <repo-url> oasis
cd oasis
make build
```

TUN 模式下需要 `sudo` 权限，请运行 `make install` 配置：

```bash
make install   # 会提示输入管理员密码，配置 sudoers 免密
```

### 仅 SOCKS5 模式（无需安装）

如果只需本地 SOCKS5 代理（不启用 TUN），编译后直接使用：

```bash
make build
./oasis config init     # 初始化配置
# 编辑 ~/.oasis/config.yaml，设置 tun.enabled: false
./oasis up              # 启动，不需要 root 权限
```

### 初始化配置

```bash
./oasis config init
```

`config init` 默认进入交互式向导；脚本场景可用 `./oasis config init --yes` 生成默认配置。

配置文件默认路径：`~/.oasis/config.yaml`

## 配置说明

```yaml
mode: rule                # 代理模式: global / rule / direct
tun:
    enabled: false         # TUN 模式（可选，false 时仅提供 SOCKS5 代理）
    device: utun           # TUN 设备名（默认 utun）
    mtu: 1500              # MTU（默认 1500）
    dns:                   # DNS 服务器列表（默认 8.8.8.8, 1.1.1.1）
        - 8.8.8.8
        - 1.1.1.1
    # via 不填时自动从 selected 节点拼接 socks5://server:port

system-proxy:
    enabled: false         # 开启后设置系统 SOCKS 代理

upstream:
    nodes:                 # 上游节点列表
        - name: My-Phone
          type: socks5
          server: 192.168.1.100
          port: 10808
    subscriptions:         # 订阅源列表
        - name: "机场A"
          url: "https://example.com/sub?token=xxx"
    selected: My-Phone     # 当前使用的节点

rules:                     # 路由规则（从上到下匹配，默认全部走 PROXY）
    - IP-CIDR,192.168.0.0/16,DIRECT
    - IP-CIDR,10.0.0.0/8,DIRECT
    - IP-CIDR,127.0.0.0/8,DIRECT
    - MATCH,PROXY
```

### 路由规则格式

每条规则格式为：`类型,参数,动作`

| 类型 | 参数示例 | 说明 |
|------|---------|------|
| `DOMAIN` | `example.com` | 精确域名匹配 |
| `DOMAIN-SUFFIX` | `google.com` | 域名后缀匹配 |
| `IP-CIDR` | `192.168.0.0/16` | IP CIDR 匹配 |
| `MATCH` | — | 兜底匹配所有流量 |

| 动作 | 说明 |
|------|------|
| `DIRECT` | 直连，不走代理 |
| `PROXY` | 通过上游节点代理 |
| `REJECT` | 阻断连接 |

### Shadowsocks 支持的加密方法

- `aes-256-gcm`
- `aes-128-gcm`
- `chacha20-ietf-poly1305`

## 使用

### 命令分组

```
oasis
├── up           启动代理
├── down         停止代理
├── status       查看运行状态
├── restart      重启代理
├── mode         查看或切换模式 (global / rule / direct)
├── node         节点管理
│   ├── list     列出所有节点
│   ├── use      切换节点
│   └── ping     延迟测试
├── config       配置管理
│   ├── init     初始化配置文件
│   ├── edit     编辑配置文件
│   ├── show     显示当前配置
│   └── reload   重载配置
├── sub          订阅管理
│   ├── update   更新订阅
│   └── status   查看订阅状态
├── tun          TUN 模式管理
│   ├── on       启用 TUN
│   ├── off      禁用 TUN
│   └── stats    查看 TUN 流量统计
├── system-proxy 系统代理管理
│   ├── on       启用 macOS 系统 SOCKS 代理
│   ├── off      关闭 macOS 系统代理
│   └── status   查看配置状态
├── logs         查看日志
├── doctor       系统诊断
├── info         显示版本信息
├── clean        清理残留进程和路由
└── version      显示版本号
```

### 常用命令

```bash
# 启动代理（守护进程模式）
oasis up

# 前台调试模式（日志直接输出到终端）
oasis up --foreground

# 停止代理
oasis down

# 查看运行状态
oasis status

# JSON 格式输出
oasis status --json

# 重启代理
oasis restart

# 切换模式
oasis mode global       # 全局模式，所有流量走代理
oasis mode rule         # 规则模式，按路由规则匹配
oasis mode direct       # 直连模式，不走代理
oasis mode              # 查看当前模式

# 查看所有节点
oasis node list

# 切换节点
oasis node use <节点名>

# 节点延迟测试
oasis node ping
```

### 配置管理

```bash
# 交互式初始化配置
oasis config init

# 跳过向导，生成默认配置
oasis config init --yes

# 编辑配置（使用 $EDITOR）
oasis config edit

# 查看当前配置
oasis config show

# JSON 格式查看
oasis config show --json

# 重载配置（运行中生效）
oasis config reload
```

### 订阅管理

```bash
# 更新订阅
oasis sub update

# 查看订阅状态
oasis sub status
```

### TUN 模式管理

```bash
# 启用 TUN
oasis tun on

# 禁用 TUN
oasis tun off

# 查看流量统计
oasis tun stats

# 开启系统 SOCKS 代理（支持系统代理的应用会保留域名走代理）
oasis system-proxy on

# 关闭系统代理
oasis system-proxy off
```

### 诊断与清理

```bash
# 系统诊断（检查配置、二进制、守护进程状态、操作系统）
oasis doctor

# JSON 格式诊断报告
oasis doctor --json

# 清理残留进程和路由
oasis clean

# 强制清理（静默执行）
oasis clean --force

# 查看日志
oasis logs

# 查看最近 50 行日志
oasis logs -n 50

# 只看错误日志
oasis logs --error
```

### 提示

- TUN 模式需要 root 权限以创建 utun 虚拟网卡和添加系统路由，请先运行 `make install` 配置 sudoers 免密
- 仅 SOCKS5 模式（`tun.enabled: false`）不需要任何 root 权限
- 系统代理适合浏览器和遵循 macOS 系统代理的应用，会把域名交给 SOCKS 上游；TUN 适合不支持代理设置的应用
- 使用 `oasis clean` 可清理 tun2socks 残留进程、DNS 重定向和系统路由，适用于异常退出后的恢复
- `oasis doctor` 提供系统的快速诊断，方便排查问题

## 模式与数据流

### 非 TUN 模式（仅 SOCKS5 代理）

```
应用 → 127.0.0.1:端口 → LocalProxy
  ├─ DIRECT → 直连目标
  ├─ PROXY  → 上游节点 (SOCKS5/Shadowsocks)
  └─ REJECT → 断开
```

无需 root 权限，无系统级网络改动。适合浏览器/App 手动配置代理的场景。

### TUN 模式（全局透明代理）

```
系统流量 → utun 虚拟网卡 → tun2socks
  ├─ global → 远程节点
  └─ rule   → LocalProxy → 规则匹配 → 远程节点 / 直连
```

1. tun2socks 创建 utun 虚拟网卡，系统路由将流量导入
2. `ifconfig` 配置设备 IP，`route` 添加系统路由表
3. tun2socks 将 TUN 层的 IP 包转为 SOCKS5 请求
4. SOCKS5 请求经规则路由（直连/代理/拒绝）后发往目标或上游节点

### 系统路由

TUN 模式启动后会在系统路由表中添加两条路由（通过 split routing 方式覆盖默认路由）：

| 目标网络 | 子网掩码 | 接口 |
|---------|---------|------|
| 0.0.0.0 | 128.0.0.0 | utun |
| 128.0.0.0 | 128.0.0.0 | utun |

DNS 请求通过 pf 重定向到本地 DNS 代理，由其经当前节点转发到上游 DNS 服务器。

### 项目结构

```
oasis/
├── cmd/oasis/
│   ├── main.go            # 程序入口
│   └── commands/
│       └── commands.go    # CLI 命令定义 (Cobra)
├── internal/
│   ├── assets/
│   │   └── assets.go      # tun2socks 二进制嵌入
│   ├── config/
│   │   └── config.go      # 配置加载/解析
│   ├── core/
│   │   └── engine.go      # 代理引擎核心（SOCKS + 可选 TUN）
│   ├── daemon/
│   │   └── daemon.go      # 守护进程 (Unix Socket IPC)
│   ├── proxy/
│   │   ├── proxy.go       # SOCKS5 协议 & 上游拨号
│   │   ├── local.go       # 本地 SOCKS5 代理（规则模式分流）
│   │   ├── dns.go         # DNS 代理 (UDP → TCP)
│   │   └── ss.go          # Shadowsocks AEAD 加解密
│   ├── router/
│   │   └── router.go      # 规则路由匹配
│   ├── subscribe/
│   │   └── subscribe.go   # 订阅解析 (Clash / ss://)
│   └── tun/
│       ├── tun.go         # macOS TUN 设备操作
│       └── dns.go         # pf DNS 重定向
├── docs/
│   └── design.md          # 设计文档
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

## License

MIT
