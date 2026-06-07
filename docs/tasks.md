# Oasis 任务列表

## 已完成

- ✅ [feat][P1] TUN 模式改为可选，SOCKS 代理无需 root 权限 — `internal/core/engine.go`
- ✅ [fix][P0] sudoers 免密不生效，去掉 bash 脚本改用独立 `sudo -n` 调用 — `internal/core/engine.go`
- ✅ [fix][P0] `AddDNS`/`RestoreDNS` 去掉 osascript 弹窗回退 — `internal/tun/tun.go`
- ✅ [fix][P1] `cleanupTUN()` 去掉 osascript 弹窗回退 — `cmd/oasis/commands/commands.go`
- ✅ [chore][P1] 构建时从 GitHub Releases 下载 tun2socks，不再提交二进制 — `Makefile`
- ✅ [chore][P2] 撰写设计文档 — `docs/design.md`
- ✅ [docs][P2] README 同步更新（TUN 可选、SOCKS-only 流程、数据流）

## 待办

### P0 修复项

- ✅ [fix][P0] `internal/tun/dns.go` 中 `EnableDNSRedirect`/`DisableDNSRedirect` 仍有 osascript fallback，sudo -n 失败时弹窗
- ✅ [fix][P0] TUN DNS 查询直连公共 DNS 导致 SOCKS 代理环境下无法访问 Google — `internal/proxy/dns.go` `internal/core/engine.go`
- ✅ [fix][P0] 上游节点 host route 使用默认网关并让 `oasis clean` 恢复 TUN DNS/路由残留 — `internal/core/engine.go` `cmd/oasis/commands/commands.go`
- ✅ [feat][P0] 增加 macOS 系统 SOCKS 代理开关，保留域名目标给手机端/上游规则代理 — `internal/systemproxy/systemproxy.go` `cmd/oasis/commands/commands.go`
- ✅ [feat][P1] `oasis config init` 改为交互式向导，默认开启系统代理、关闭 TUN — `cmd/oasis/commands/commands.go` `internal/config/config.go`
- ✅ [chore][P2] 配置字段从 `system_proxy` 统一为 `system-proxy`，读取时兼容旧字段 — `internal/config/config.go`
- ✅ [feat][P1] 增加 `oasis sub add <name> <url>`，支持 `--update` 添加后立即拉取节点 — `cmd/oasis/commands/commands.go`

### P2 优化项

- [chore][P2] `go vet` 报 IPv6 地址格式警告 4 处 — `proxy.go:115,139` `commands.go:261,657`
- [chore][P2] 确认 `docs/design.md` 中无敏感信息（代理地址等）
- [chore][P2] 确认 Makefile 中无硬编码代理地址

### P3 待定

- [docs][P3] `oasis doctor` 增加 tun2socks 二进制检测
- [feat][P3] `oasis down` 自动清理 tun2socks 残留进程
