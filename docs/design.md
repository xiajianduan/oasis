# Oasis 设计文档

## 项目定位

Oasis 是一个用 Go 编写的轻量级代理 CLI 工具，专为 macOS 设计。核心能力：

- 提供本地 SOCKS5 代理入口，支持规则路由（DOMAIN / IP-CIDR 匹配）
- 可选 TUN 模式，将系统级流量透明转发到上游代理
- 支持 SOCKS5 和 Shadowsocks (AEAD) 两类上游节点
- 守护进程模式，Unix Socket IPC 通信

---

## 架构总览

```
┌──────────────────────────────────────────────────────┐
│                    CLI (Cobra)                        │
│  cmd/oasis/commands/commands.go                      │
│  up / down / status / mode / node / config / tun ... │
└──────────┬───────────────────────────────────────────┘
           │
           ▼
┌──────────────────┐    ┌──────────────────────────────┐
│   Daemon         │    │  Engine                      │
│  internal/daemon │◄──►│  internal/core/engine.go     │
│  Unix Socket IPC │    │  启动/停止/重载              │
│  pid/socket 文件 │    │  管理 localProxy, tun2socks  │
└──────────────────┘    └──┬───────────────────────┬───┘
                           │                       │
                           ▼                       ▼
              ┌────────────────────┐    ┌──────────────────────┐
              │  LocalProxy        │    │  TUN 子系统           │
              │  internal/proxy/   │    │  internal/tun/        │
              │  SOCKS5 入站       │    │  utun 设备 / pf DNS   │
              │  规则分流          │    │  路由表 / 系统 DNS    │
              │  上游拨号          │    │                      │
              │  (SOCKS5/SS)       │    │  tun2socks (外部进程) │
              └────────────────────┘    └──────────────────────┘
```

---

## 模块详解

### 1. CLI 层 — `cmd/oasis/commands/commands.go`

基于 Cobra 的命令行接口，所有用户交互入口。命令分组：

| 命令 | 功能 |
|------|------|
| `up` | 启动代理（守护进程或前台） |
| `down` | 停止代理，清理 TUN 残留 |
| `status` | 查看运行状态 |
| `restart` | 重启 |
| `mode` | 切换 direct / rule / global |
| `node list/use/ping` | 节点管理 |
| `config init/edit/show/reload` | 配置管理 |
| `sub update/status` | 订阅更新 |
| `tun on/off/stats` | TUN 开关与流量统计 |
| `system-proxy on/off/status` | macOS 系统 SOCKS 代理开关 |
| `logs / doctor / info / clean` | 日志/诊断/清理 |

### 2. 引擎 — `internal/core/engine.go`

核心状态机，管理所有子组件生命周期。定义三种模式：

- **direct** — 空跑，不做任何代理
- **rule** — 启动本地 SOCKS5 代理，按规则分流
- **global** — 直接 TUN 全局转发（无规则）

TUN 为可选子系统，不启用时 rule 模式仍可独立运行。系统代理也是可选子系统，启用后设置 macOS SOCKS 代理：rule 模式指向 LocalProxy，global 模式直接指向当前 SOCKS5 节点。

```
Start()
  ├─ direct → 标记 running，返回
  ├─ rule → 启动 LocalProxy（随机端口，127.0.0.1）
  └─ TUN.Enabled?
       ├─ 否 → 完成，SOCKS 代理正常运行
       └─ 是 → 启动 DNSProxy（经当前节点查询）→ pf 重定向 → 系统 DNS
                └─ goroutine: startTUN()
                     ├─ 查找 tun2socks 二进制
                     ├─ sudo -n 启动 tun2socks
                     ├─ 轮询检测 utun 设备
                     ├─ sudo -n ifconfig 配置 IP
                     └─ sudo -n route 添加路由
```

### 3. 代理 — `internal/proxy/`

#### LocalProxy (`local.go`)
- 监听 `127.0.0.1:0`（随机端口）
- SOCKS5 握手解析目标地址
- 通过 Router 匹配规则 → DIRECT / PROXY / REJECT
- PROXY 动作通过 DialUpstream 拨号到上游节点

#### DNSProxy (`dns.go`)
- UDP 监听随机端口
- 将 DNS 查询转为 TCP 转发到上游 DNS 服务器
- rule/global 模式下 DNS TCP 查询通过当前选中节点发出，避免公共 DNS 直连失败
- 仅在 TUN 模式下启用

#### SOCKS5 / Shadowsocks (`proxy.go`, `ss.go`)
- SOCKS5 协议客户端实现（CONNECT 命令）
- Shadowsocks AEAD 加密（aes-256-gcm / aes-128-gcm / chacha20-ietf-poly1305）
- 支持 SOCKS5 用户名密码认证

### 4. 路由 — `internal/router/router.go`

线性匹配引擎，规则顺序即优先级：

```
DOMAIN → DOMAIN-SUFFIX → IP-CIDR → MATCH
```

每个规则映射到动作：DIRECT（直连）/ PROXY（代理）/ REJECT（拒绝）

### 5. TUN 子系统 — `internal/tun/`

#### tun.go
- macOS utun 虚拟网卡创建（AF_SYSTEM socket）
- ifconfig IP 配置
- route 添加/删除

#### dns.go
- pf 规则实现 DNS 重定向（`127.0.0.1:53` → DNSProxy 端口）
- 通过 `sudo -n /sbin/pfctl` 操作

### 6. 守护进程 — `internal/daemon/daemon.go`

- Unix Domain Socket IPC（`~/.oasis/oasis.sock`）
- PID 文件管理（`~/.oasis/oasis.pid`）
- 支持 shutdown / status / stats / reload 命令
- SIGINT/SIGTERM 优雅退出

### 7. 配置 — `internal/config/config.go`

YAML 配置，默认路径 `~/.oasis/config.yaml`：

```yaml
mode: rule                # global / rule / direct
tun:
    enabled: false         # TUN 可选，默认关闭
    device: utun
    mtu: 1500
    dns: [8.8.8.8, 1.1.1.1]
system-proxy:
    enabled: false
upstream:
    nodes:
        - name: node1
          type: socks5
          server: ...
          port: 1080
    subscriptions:
        - name: sub1
          url: https://...
    selected: node1
rules:
    - IP-CIDR,192.168.0.0/16,DIRECT
    - MATCH,PROXY
```

### 8. 订阅解析 — `internal/subscribe/subscribe.go`

支持两种格式：
- Clash YAML（`proxies:` 字段）
- `ss://` 分享链接（BASE64 编码）

---

## 数据流

### 非 TUN 模式（仅 SOCKS5 代理）

```
应用 → 127.0.0.1:SOCKS_PORT → LocalProxy
  ├─ DIRECT → 直连目标
  ├─ PROXY  → DialUpstream(node, target)
  │            ├─ SOCKS5 → 上游 SOCKS5 服务器
  │            └─ SS     → AES/GCM 加密 → 上游 SS 服务器
  └─ REJECT → 断开连接
```

无需 root 权限，无系统级网络改动。

### 系统代理模式

```
支持系统代理的应用 → macOS SOCKS 系统代理
  ├─ rule   → LocalProxy → 规则匹配 → 远程节点/直连
  └─ global → 当前 SOCKS5 节点
```

系统代理保留域名目标，适合依赖域名规则的上游代理；TUN 透明代理通常只看到目标 IP，适合不支持代理设置的应用。

### TUN 模式（全局透明代理）

```
系统流量 → utun 虚拟网卡 → tun2socks
  ├─ global → tun2socks → 远程节点
  └─ rule   → tun2socks → LocalProxy → 规则匹配 → 远程节点/直连
```

依赖：
- utun 设备创建（root）
- 路由表修改（root）
- pf DNS 重定向（root）
- 系统 DNS 设置（root）

---

## 安全模型

### sudoers 免密机制

`make install` 在 `/etc/sudoers.d/oasis` 注册以下命令免密：

```sudoers
ALL ALL=(ALL) NOPASSWD:
    /sbin/ifconfig,
    /sbin/route,
    /usr/bin/pkill,
    /usr/sbin/networksetup,
    /sbin/pfctl,
    /usr/local/libexec/oasis-tun2socks
```

运行时通过 `sudo -n` 调用以上命令，不弹 GUI 密码框。

### 提权流程

```
非 root 用户运行:
  startTUN() → sudo -n /usr/local/libexec/oasis-tun2socks ...
               sudo -n ifconfig utunX ...
               sudo -n route add ...
  AddDNS()   → sudo -n /usr/sbin/networksetup -setdnsservers ...
  DNS rdr    → sudo -n /sbin/pfctl ...

root 用户运行:
  所有命令直接执行，跳过 sudo
```

### tun2socks 二进制管理

tun2socks v2.6.0 作为 Go module 依赖，通过 `go install` 下载编译后嵌入程序：

```
make build → go install github.com/xjasonlyu/tun2socks/v2@v2.6.0
           → cp to internal/assets/tun2socks_darwin_amd64
           → go build（//go:embed 编译进二进制）
```

运行时查找优先级：
1. `/usr/local/libexec/oasis-tun2socks`（sudoers 注册路径）
2. 从 embed 解压到 `/tmp/oasis-tun2socks`
3. 可执行文件同目录的 `bin/tun2socks`
4. CWD 的 `bin/tun2socks`
5. `$PATH` 中的 `tun2socks`

---

## 运行时文件布局

```
~/.oasis/
├── config.yaml       # 用户配置
├── oasis.sock        # Unix Socket（运行中）
├── oasis.pid         # PID 文件（运行中）
└── oasis.log         # 日志（运行中）

/usr/local/
├── bin/oasis         # 安装后的主程序
└── libexec/oasis-tun2socks  # 安装后的 tun2socks

/etc/sudoers.d/oasis  # sudoers 配置（make install 创建）
```

---

## 构建与安装

```bash
# 开发构建（自动下载 tun2socks）
make build

# 安装到系统（配置 sudoers、注册路径）
make install

# 仅下载 tun2socks 二进制
make tun2socks-download
```

### 网络要求

`make build` 从 GitHub Releases 下载 tun2socks 预编译二进制。如在受限网络环境，请自行配置代理。

---

## 设计决策记录

### 1. TUN 可选而非必选

**问题**：原设计强制 TUN 模式，SOCKS 代理被绑定在 TUN 子系统上，导致纯 SOCKS 场景也必须提权。

**决策**：TUN 与代理模式正交。`startLocked()` 中 TUN 子系统为独立 if 分支，不启用时仅运行 LocalProxy，无需任何 root 权限。

### 2. 用独立 sudo 调用替代 bash 脚本

**问题**：原 `startTUN()` 将所有特权操作打包进一个 bash 脚本，通过 `sudo -n /bin/bash script.sh` 执行。但 sudoers 注册的是具体二进制路径（`/usr/local/libexec/oasis-tun2socks`、`/sbin/ifconfig`、`/sbin/route`），不是 `/bin/bash`，导致 `sudo -n` 失败后回退到 osascript GUI 弹框。

**决策**：每个特权命令单独使用 `sudo -n` 调用，sudoers 精确匹配，不再回退到 osascript。

### 3. tun2socks 不提交到 git

**问题**：10MB 二进制直接提交到 git 仓库。

**决策**：通过 `go install` 在构建时自动下载 + 编译，`//go:embed` 嵌入。git 中只保留代码。

### 4. 去 osascript 回退

**问题**：`AddDNS`/`RestoreDNS`/`DisableDNSRedirect` 在 `sudo -n` 失败时回退到 osascript GUI 弹框，用户困惑。

**决策**：要求通过 `make install` 完成 sudoers 配置，运行时只使用 `sudo -n`，失败直接报错。不再静默弹框。
