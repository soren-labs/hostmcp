# v0.2.0 交付验收

验证日期：**2026-10-28 UTC**。本次围绕独立 EC2 Hub，从实际 MCP 客户端经过
HTTPS、账户节点连接，验证原生系统操作。公开记录使用角色描述；不提交真实
域名、IP、私有账户名、邀请、token、会话标识或客户端原始日志。

## 部署与原生环境

Hub 部署在专用 **AWS EC2 t3.micro（1 GiB RAM、2 vCPU）**，Ubuntu 24.04，
8 GiB 加密系统盘。Caddy 负责公开 HTTPS，Hub 只监听回环地址，Hub 本身使用
专用普通账户运行。Hub 与本机 worker 都安装为开机启动的 systemd 服务。
这次验证了真实 Caddy TLS 和远端 MCP，不只是生成反向代理模板。

| 执行环境 | 原生架构 | 常驻账户节点 | 验证重点 |
| --- | --- | --- | --- |
| Mac mini | darwin-arm64 | 当前登录账户，UID 501 | LaunchAgent、跨公网 MCP、实际系统账户 |
| Ubuntu 工作站 1 | linux-amd64 | 专用 worker，UID 997 | systemd、普通账户、主动连接 |
| Ubuntu 工作站 2 | linux-amd64 | worker UID 1001；root UID 0 | 同机账户分离、不同客户端范围、原生文件权限 |
| GCP 实例 | linux-amd64 | 专用 worker，UID 996 | 云节点接入，运维连接经 Tailscale |
| EC2 Hub 所在实例 | linux-amd64 | 专用 worker，UID 995 | Hub 与执行账户分离、子代理验证 |

以上是 **五台真实机器、六个常驻账户节点**。普通账户的实际 sudo 权限仍属于
操作系统权限；不能仅凭 UID 非零就宣称它没有管理员能力。测试用 Linux worker
明确没有免密 sudo。Mac 常驻节点沿用现有账户，不把它标为无提权能力的沙箱。

另外启动了 **三个真实 Modal 临时容器**，以专用 UID 1000 运行 Agent。三者
都经正式 HTTPS Hub 完成身份查询、命令执行和文件写读。验证后已停止容器、
撤销节点和临时客户端凭证、删除邀请副本；它们不属于交付后常驻资源。

## 不同 Agent 的真实调用

| 调用方 | 授权范围 | 真实工具结果 |
| --- | --- | --- |
| Claude Code 2.1.292 / Opus 5.5 | 工作站 2 的 root 节点 | `list_devices`、`device_info`、`exec_command` 成功；`id -u` 输出 `0` |
| Codex CLI 0.162.1 | 同一工作站的 worker 节点 | 三个工具成功；`id -u` 输出 `1001` |
| 独立开发子代理 | 仅 EC2 worker | UID/EUID 995；命令和 62-byte 文件往返成功；root 节点调用未派发；免密 sudo 被系统拒绝 |

两个本地 Agent 都使用各自的 token 文件，经 `hostmcp connect` stdio bridge
连接远端 Hub；没有修改它们原有全局 MCP 配置。原始 JSONL 的工具调用与结果
对应关系已检查，脱敏证据见
[agent-clients-2026-10-28.json](validation/agent-clients-2026-10-28.json)。

Codex 首次在 `approval_policy=never` 下拒绝命令执行；此结果被保留。
随后通过客户端正常 `--approve-for-me` 审批流程完成验收，没有使用跳过审批
或 sandbox 的参数。HostMCP 的节点授权不会覆盖客户端自身的审批策略。

首次执行响应可能已经包含输出但仍为 `running:true`。验收继续使用相同认证
主体和设备调用 `write_stdin`，确认退出码 0；不以模型总结替代真实工具证据。

## 系统权限、服务与凭证

`scripts/verify-access.py` 经正式公网 Hub 验证了以下行为，运行耗时 28.632s：

- root 与 worker 节点实际属于同一 hostname，身份查询的 UID/EUID 和执行身份一致。
- worker 凭证只列出授权节点，无法调用 root 节点。
- root 能读取 root-owned `0600` fixture；worker 通过文件工具和 Shell 都被
  操作系统拒绝。测试 fixture 随后清理。

Linux systemd 实测包括普通账户 Hub、root/worker 两个 Agent 的安装启动、
HOME/SHELL/工作目录、凭证交叉访问拒绝，以及重复卸载两次。临时服务全部清理。

macOS 系统服务实测使用**仅监听回环地址的临时 Hub**和 root LaunchDaemon。
真实 MCP 命令返回 UID 0、HOME `/var/root`、SHELL `/bin/sh`，root 可读取私有
fixture，普通账户被拒绝。两个 LaunchDaemon 各卸载两次成功，进程和临时配置、
密钥、日志、fixture 均清理。没有因此向公开 Hub 增加 Mac root 节点。

本轮修复了实测发现的配置文件原子替换所有权、注册节点 ID，以及 macOS
补充组身份预检问题。邀请的 expected user 检查用于发现误部署；它不构成对
已被攻陷设备的可信身份证明。

凭证维护在运行中的 HTTPS Hub 上以短期、普通节点范围凭证实测：

| 操作 | 验证结果 |
| --- | --- |
| 创建 | 只能列出指定的普通账户节点 |
| 更新范围 | 同一 token 即时改为新节点范围，不重启 Hub |
| 轮换 | 保留客户端 ID 和范围；旧 token 的 HTTP 请求返回 401，新 token 可正常调用 |
| 撤销 | 新 token 的 HTTP 请求也返回 401 |
| 清理 | 临时凭证文件删除，测试客户端撤销 |

管理命令在 Hub 本机以 Hub 服务账户执行；owner 凭证保留在 Hub，没有复制给
本地 Agent 或开发子代理。服务安装、Modal、子代理和凭证维护的脱敏汇总见
[delivery-2026-10-28.json](validation/delivery-2026-10-28.json)。

## 低配 EC2 的 200 个模拟节点

测试启动了独立的临时 Hub 进程，与正式服务隔离。模拟器持有 200 个独立临时
节点凭证并建立 WebSocket 连接；它验证连接、路由与重连，不执行 200 台机器
上的原生任务。

| 项目 | 实测 |
| --- | ---: |
| 同时在线模拟节点 | 200 |
| 成功路由 | 200 |
| 请求并发 | 8 |
| 断开后重连节点 | 20 |
| 重连后的成功路由 | 20 |
| 测量窗口 | 3.012 秒 |
| Hub RSS 采样峰值 | 32,440 KiB，约 31.7 MiB |
| Hub CPU 时间 | 1.63 秒 |
| 平均 CPU，单逻辑核为 100% | 54.63% |
| `/proc` 样本 | 31 |

测量只计临时 Hub 进程，排除模拟器、Caddy 和设备工作负载。进程与凭证在测试
结束后清理。原始数值见
[ec2-scale-2026-10-28.json](validation/ec2-scale-2026-10-28.json)。

这是短时测试，不是长期容量保证，也不代表 200 个并行命令。Hub 当前保留
8 个在途 HTTP 请求和 8 个并发设备握手的轻量上限；超过时返回 503。测试曾以
16 并发触发该边界，随后按现有设计验证，没有为得到通过结果调大上限。

可选复现方式：先构建独立 Hub binary，再设置 `HOSTMCP_SCALE_BINARY` 运行
`go test -run '^TestScaleAccountNodes$' -v ./internal/hub`。默认 CI 跳过该项；
Linux 支持独立进程资源采样，其他系统仍可验证路由行为。

## 发布验证与范围

Mac 集成分支已通过完整 `go test -race ./...`、`go vet ./...` 和参考源码完整性
检查；62 个上游参考文件保持原始字节。Linux 原生账户与系统服务的测试已通过。
GitHub 的 Linux/macOS 流水线会对最终提交再次执行 race、vet、构建和九工具
stdio 验收；发布流水线还验证归档安装及损坏归档拒绝。最终运行状态以
[Actions](https://github.com/soren-labs/hostmcp/actions) 和
[v0.2.0 Release](https://github.com/soren-labs/hostmcp/releases/tag/v0.2.0) 为准。

版本提供 linux-amd64、linux-arm64、darwin-amd64、darwin-arm64 四种归档。
本次真实机器覆盖 darwin-arm64 和 linux-amd64；其他两种为交叉构建目标。

仍保留以下边界，避免将原型声明为尚未验证的产品能力：

- 一个节点就是一个机器/原生账户。该账户拥有 sudo 权限时，相应能力也在它
  的原生权限范围内；不增加命令白名单、目录权限层或隐藏提权代理。
- 终端和幂等记录在 Agent 内存中。Hub/网络恢复后可以访问仍存活 Agent 的
  session；Agent 进程重启不能恢复旧终端，也没有持久 exactly-once 保证。
- 当前是单个信任域的静态 bearer 服务；OAuth-only 宿主、Windows、桌面控制
  不在范围。Claude Desktop 与 ChatGPT 云端宿主没有完成本轮验收。
- macOS TCC/SIP 与操作系统自身限制仍有效。
- EC2 公开入口使用与当前公网 IP 对应的 DNS 名称。没有交付自有域名或
  Elastic IP；实例停止再启动后的地址变化需要同步处理 DNS 和 Hub public URL。
- 本轮没有长期耐久、磁盘故障恢复或跨地域故障切换测试。
