# 验证记录

验证日期：**2026-10-22 UTC**。测试从实际 MCP 客户端经过公网 HTTPS、Hub、
设备连接到原生系统操作；另外运行单元/集成测试与竞态检测。所有写入使用
HostMCP 专属临时目录，不访问正在运行的 Chat On Steroids 安装或其用户数据。

## 实际设备

| 角色 | 系统 / 架构 | 执行身份 | 已执行 |
| --- | --- | --- | --- |
| VPS Hub + Agent | Ubuntu 24.04 系列 / linux-amd64；2 vCPU，412.27 MiB RAM，无 swap | 普通用户，UID/EUID 1000 | 原生用户服务、Cloudflare 公网接入、九工具验收 |
| 家庭 Ubuntu Agent | Ubuntu 26.04.1 LTS / linux-amd64 | 普通用户，UID/EUID 1000 | 原生用户服务、Go race/vet、九工具验收 |
| Mac Agent + MCP 客户端 | macOS Darwin 27 / darwin-arm64 | 普通用户，UID/EUID 501 | LaunchAgent、Go race/vet、本地 stdio、公网九工具、Claude Code |

公开记录不包含真实地址、域名、用户名、邀请或凭据。身份工具的 UID/EUID 与
实际执行的 id -u 一致。服务的 SHELL 分别保留 bash 与 zsh；没有修改 sudoers。

## 验收结果

| 检查 | 状态 | 证据与范围 |
| --- | --- | --- |
| 参考源码完整性 | passed | 62 文件、722,204 bytes，固定上游 f51acbccdd734f524799ea92bb747be765fba1e4；SHA-256 全一致 |
| macOS 原生测试 | passed | Go 1.27.3；go test -race ./... 与 go vet ./... 通过；连接取消回归另行通过 |
| Ubuntu 原生测试 | passed | Go 1.27.3；全包 race/vet 通过；最终变更的 host/service 与 agent 连接取消回归补验通过；其它文件哈希未变 |
| GitHub Linux/macOS CI | passed | [c9d2b41 两平台运行](https://github.com/soren-labs/hostmcp/actions/runs/39776684301)通过，包含真实 stdio 九工具；后续合并和发布结果见 Actions |
| 本地 stdio | passed | 真实 JSON-RPC initialize、tools/list、tools/call；2025-11-25；九工具全部执行 |
| 三设备公网 HTTP MCP | passed | Mac、家庭 Ubuntu、VPS 分别九工具全部执行；协议 2025-11-25；各自临时目录清理成功 |
| stdio 到远程 Hub 的桥接 | passed | Mac 真实 hostmcp connect；九工具全部执行；17.723s；清理成功 |
| MCP 版本协商 | passed | 官方 Go SDK v1.8.0 集成测试覆盖 2025-06-18、2025-11-25、2026-07-28 的工具发现和真实 WebSocket 路由调用 |
| Claude Code | passed | 2.1.292；stdio connect 桥；真实初始化 connected；1 次 list_devices + 3 次 device_info 对应 4 个成功 tool_result |
| 公网认证与设备范围 | passed | 无 token/错误 token 为 401；非法 Origin 为 403；Ubuntu scope 只显示/访问 Ubuntu，访问 Mac 被拒；撤销后 401 |
| 设备认证与撤销 | passed | 集成测试覆盖一次性邀请、凭据角色隔离、设备身份匹配、撤销关闭连接、重复连接替换清理 |
| Hub 重启与在途命令 | passed | 3ba9919 修复后三机真实复验，39.246s；在途调用明确 unknown outcome，三台自动重连；原 session 的 request_id 重试复用，stdout 完整保留 survived，marker 仅 started/finished 两行 |
| systemd / launchd 服务 | passed | 三台实际安装和激活；Linux 注销后用户服务持续运行；Mac LaunchAgent 在当前 GUI 登录会话运行 |
| systemd 路径和失败恢复 | passed | Ubuntu systemd 259 真实解析特殊字符路径；检查安装后运行状态；Mac 未加载的生成 plist 能安全卸载 |
| 家庭 Ubuntu 作为 Hub | partial | 独立回环 Hub 经 Quick Tunnel 的公网 healthz、一次性邀请注册、Agent 公网连接成功；Mac 对新域名 initialize 遇到 EOF，跨机器写读尚未完成；本轮服务与秘密已清理 |
| 安装器与发布脚本 | passed | 语法检查；四平台本地交叉构建；正确 archive 安装；篡改、缺失/重复校验值及符号链接 binary 被拒 |
| Release 资产 | passed | [v0.1.0](https://github.com/soren-labs/hostmcp/releases/tag/v0.1.0) 已公开，commit d4288e87da57；[发布流水线](https://github.com/soren-labs/hostmcp/actions/runs/39777158515) 双平台验证、四平台构建、安装校验及发布全部通过；四包 API SHA-256 与清单一致 |
| 公开 Release 安装实机验收 | partial | Mac 已从固定 tag 安装脚本公开下载并校验 v0.1.0 / d4288e87da57，LaunchAgent running；两台 Linux 的最终升级状态待本地连接恢复后回读，未将未知结果标为通过 |
| Named Tunnel、自有域名 Caddy、Docker Hub | not tested | 已提供配置和步骤；未将模板语法检查标为实机部署通过 |
| ChatGPT 云端、其它 MCP 宿主 | not verified | 不从通用协议测试推断任意宿主兼容；首版没有完整 OAuth 登录服务 |

## 九工具验收内容

可复现的脚本为 scripts/smoke.py，仅依赖 Python 标准库和现成 hostmcp binary：

```sh
python3 scripts/smoke.py --binary ./build/hostmcp --mode stdio

python3 scripts/smoke.py --binary ./build/hostmcp \
  --url https://mcp.example.com/mcp --token-file /absolute/path/client.token \
  --devices mac,ubuntu,vps

python3 scripts/smoke.py --binary ./build/hostmcp --mode bridge \
  --url https://mcp.example.com/mcp --token-file /absolute/path/client.token \
  --devices mac
```

脚本执行身份/cwd/env 校验，中文与空格路径读写、文本补丁、Base64 二进制往返，
有效 PNG 的 MCP image content，PTY 中文交互与尺寸更新，非零退出码，长任务
清单与续读，执行超时，显式终止，以及 request_id 重试只追加一次文件的检查。

HTTP 逐设备首轮耗时为 Mac 42.743s、Ubuntu 44.464s、VPS 35.053s。这是包含
网络往返和 fixture 等待的功能验收时间，不是性能基准或吞吐量承诺。

单元/集成回归还覆盖 FIFO/特殊文件不阻塞、输出边界、会话回收、跨主体会话拒绝、
幂等 tombstone、补丁预检和部分失败、umask 与现有权限保留、连接心跳、并发配置
更新及 HTTP Origin/Host 验证。所有写入都符合执行用户已有的系统权限。

## 低配 VPS 资源测量

测量对象为 2 vCPU、422,168 KiB（412.27 MiB）RAM、无 swap 的真实 VPS。
它同时运行已有服务；没有为测试停掉其它应用。RSS 只统计对应进程，
不把设备上执行的任意编译/应用内存算进 Hub。

| 场景 | Hub RSS 峰值 | VPS Agent RSS 峰值 | cloudflared RSS 峰值 | 平均 CPU：Hub / Agent / Tunnel | 系统 MemAvailable 范围 |
| --- | ---: | ---: | ---: | --- | ---: |
| Hub 启动、尚无设备连接（首次单点样本） | 11,008 KiB | 未运行 | 22,940 KiB | 未测量 | 未测量 |
| 三设备在线、功能验收后空闲 | 12,388 KiB | 11,052 KiB | 19,964 KiB | 0.00% / 0.00% / 0.00% | 95,236–96,828 KiB |
| 三设备同时输出各 256 KiB 并等待 4 秒 | 15,820 KiB | 13,116 KiB | 20,132 KiB | 1.28% / 0.14% / 1.00% | 88,024–96,300 KiB |

首次单点样本时间为 2026-10-22 11:54:50Z。最终采样始于 12:13:51Z，测量
三个 systemd 用户服务 MainPID 的 `/proc` RSS 和 CPU 时间，间隔为 250ms。
空闲阶段为 9 个样本、跨度 2.008s；受控任务阶段为 29 个样本、跨度 7.026s。
CPU 百分比以一个逻辑 CPU 为 100%，由进程 CPU tick 增量计算；空闲短窗口
未观察到 CPU tick 增长，并不表示长期 CPU 使用为零。

三条命令经真实公网 MCP 同时派发，不写持久文件。Mac、Ubuntu、VPS 分别运行
4.185s、4.041s、4.018s；客户端调用及轮询整体耗时 7.230s。每台均实际接收
262,144 bytes，内容逐字节核对，退出码为 0，没有超时或丢弃输出。
同一次采样中三个服务 RSS 合计峰值为空闲 43,404 KiB（42.39 MiB）、
受控任务 48,968 KiB（47.82 MiB）。这不包含子命令及其它应用的内存。

该 VPS 保留已有服务；同期可能有独立家庭 Hub 验证的临时传输。因此系统
MemAvailable 不是 HostMCP 独占内存增量，进程 RSS 是本次主要证据。
实际运行 binary 的 SHA-256 为
`acced916300d711555f1e4e720e4e55fcde0841cdcc5f86af40a529a6fb08a93`。
原始时间序列、运行条件与逐设备结果保留在本地 ignored
`build/validation/resource-measurements.json`。
这些数字是该机器该时刻的实测值，不是所有部署的最低内存保证。没有完成长期
耐久、断电恢复或大规模多设备压力测试。

## 部署与会话边界

本次公网入口使用 **Cloudflare Quick Tunnel**，从 Mac 通过真实外网域名调用。
它验证 HTTPS 与设备 WebSocket 的整条连接链；它不是固定生产域名的验收。
Quick Tunnel 没有 uptime 保证，重新创建会改变域名，也不支持 SSE。当前 Hub
采用 JSON 短响应与独立设备 WebSocket；长命令通过 session 查询。

生产部署使用 named Tunnel 或自有域名 HTTPS。需要相应账户、域名与发布配置；
本次没有替用户绑定固定域名，也没有宣称完成该项验收。

- 会话在设备 Agent 内存中。Hub/网络重连可以找回仍在运行的 Agent 会话；
  Agent 进程重启不能恢复旧终端、未读取输出或幂等记录。
- 网络故障后命令可能已经执行。服务不自动重放，调用方应查询实际状态。
  request_id 是有保留期和容量上限的内存记录，不是永久 exactly-once 保证。
- 托管进程终止作用于其 POSIX 进程组，故意自行脱离会话的 daemon 不在该组内。
- 静态 bearer、单个信任域；没有完整 OAuth、多租户隔离、计费或 SaaS 账户。
- 没有 GUI 键鼠、屏幕捕获或浏览器自动化；view_image 读取已有图片。
- macOS 同 UID 不等于绕过 TCC/SIP；两种系统均保留正常权限错误。
- linux-arm64 与 darwin-amd64 发布目标经过交叉构建；本次没有对应物理机原生验收。
