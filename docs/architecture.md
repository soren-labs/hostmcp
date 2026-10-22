# 架构与实现决策

## 产品边界

HostMCP 是由部署者拥有和控制的多设备 MCP 服务。第一版服务一个所有者或同一
信任域。一个 Hub 对外暴露 MCP，各个设备 Agent 主动连入；模型选择、任务规划
和客户端审批由外部 MCP 客户端负责。

| 部件 | 责任 | 执行身份 |
| --- | --- | --- |
| Hub | MCP、调用者认证、设备登记、路由、撤销 | Hub 服务账户 |
| Device Agent | 文件、Shell、PTY、托管进程 | 启动 Agent 的 Linux/macOS 用户 |
| stdio 本地模式 | 直接提供当前机器的工具 | 启动 MCP 子进程的客户端用户 |
| connect 桥 | 将本地 stdio MCP 转发至远程 Hub | 仅持有客户端凭证 |
| TLS 入口 | HTTPS、域名、可选 Cloudflare Tunnel | 独立网络组件 |

Hub 不包含本地命令执行器。若需要管理 Hub 机器本身，必须单独注册该机器上的
Agent。这样，部署在小 VPS 上的 Hub 只负责控制和转发，构建、下载、包安装等
工作负载由选中的设备承担。

## 为什么使用 Go

历史提取计划优先考虑 Node/TypeScript。完成源码检查后，当前实现改用 Go：

- 一个可执行文件可兼任 Hub、Agent、stdio 服务和部署 CLI。
- 发布 linux/amd64、linux/arm64、darwin/amd64、darwin/arm64 四种产物。
- 运行设备不必安装 Node、npm、Python 或 Electron；Python 只用于源码校验和
  发布打包，Go 只在构建时需要。
- 基于官方 MCP Go SDK 使用标准协议实现，WebSocket 用于内部 Hub-Agent 通道。
- 原生 POSIX 进程和 PTY 能直接继承系统用户身份，无须容器来承载设备能力。

这是部署复杂度与资源使用的工程选择，不能从语言选型直接推导最低内存。实际
RSS、空闲 CPU 和并发表现需在目标机器上测量，记录在 validation.md。

依赖版本以 go.mod/go.sum 为准。参考 TypeScript 文件保持不可修改，不进入 Go
运行时构建，也不声称其上游测试已经通过。

## 两条独立通道

外部通道为 MCP 客户端到 **https://域名/mcp** 的 Streamable HTTP；可用本地
stdio 桥接不支持直连配置的客户端。内部通道由设备主动发起 WebSocket 连接，
携带单独的设备身份，再承载工具请求和结果。

外部 MCP bearer 不是设备凭证。邀请凭证只用于加入，完成后设备使用持久凭证。
调用者允许访问的 device ID 与连接设备的身份由 Hub 判断，不由工具参数自报
身份决定。终端会话也要绑定调用主体与设备，不能只凭 session ID 访问。

Hub、Agent 和 MCP 协议各自有不同版本；不要将内部 WireVersion 或产品 0.1.0
当成 MCP 协议版本。已协商的协议版本和实际客户端验证记录应单独记录。

## 状态与失败语义

配置、token 与邀请都应放在仅该账户可访问的目录中，不进入 Git。Hub 配置保存
设备及访问凭证的授权状态；Agent 配置保存自己的连接身份。设备掉线时，后续
调用应清楚失败，不能偷偷切换到另一台机器执行。

网络断开不等于命令撤销。已派发命令可能已经执行，即使客户端没拿到结果。
有副作用的失败调用应先检查实际状态；不要自动重放。进程会话是设备上的运行
状态，当前版本不承诺 Hub/Agent 重启后恢复全部 PTY 和输出。具体保留、清理与
退出行为由运行时测试记录确认。

设备连接应支持心跳和退避重连。重连解决连接可用性，不提供分布式事务或
exactly-once 执行语义。完整的持久任务恢复、离线排队和多 Hub 高可用不属于
首版承诺。

## 权限模型

设备 Agent 使用普通系统路径、用户 UID/GID、附加组和可用的环境。用户本来
无权访问的文件会返回系统错误；其已被系统允许的文件不再经过项目目录白名单。
服务环境和交互 Shell 的 PATH 不一定相同，应通过 doctor 与实际命令确认。

Linux 的 sudo、SELinux/AppArmor、文件权限仍然有效。macOS 的 TCC、SIP、
Automation、Accessibility、屏幕录制与桌面登录状态仍然有效。Agent 不是通用
权限绕过机制；使用 root 启动是部署者选择的另一种身份，不是安装默认行为。

用户服务是默认部署方式。Hub 可放进容器；设备 Agent 应原生运行，否则看到
的用户、文件系统和进程环境主要是容器中的环境。

## 官方依据与版本说明

以下资料于 2026-10-22 核对；它们说明设计边界，不代替本项目的兼容性实测。

- [MCP Streamable HTTP 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)
  与 [2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)：
  协议 session、GET stream 和元数据规则有差异，必须按实际 SDK/客户端测试。
- [MCP authorization](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization)：
  标准 OAuth 发现与 token 验证；静态 bearer 不是完整 OAuth 实现。
- [Apple Daemons/Agents](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/DesigningDaemons.html)：
  系统 daemon 与用户会话不同。
- [Apple 文件访问控制](https://support.apple.com/guide/security/controlling-app-access-to-files-secddd1d86a6/web)：
  文件与自动化权限仍由 macOS 管理。
- [systemd.exec](https://www.freedesktop.org/software/systemd/man/260/systemd.exec.html)：
  服务用户、附加组、环境与隔离配置会影响进程实际权限。
