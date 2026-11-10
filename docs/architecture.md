# 架构与实现决策

## 产品边界

RMC 是由部署者拥有和控制的多设备 MCP 服务。第一版服务一个所有者或同一
信任域。一个 Hub 对外暴露 MCP，各个设备节点主动连入；模型选择、任务规划
和客户端审批由外部 MCP 客户端负责。

| 部件 | 责任 | 执行身份 |
| --- | --- | --- |
| Hub | MCP、调用者认证、设备登记、路由、撤销 | Hub 服务账户 |
| 设备节点 | 文件、Shell、PTY、托管进程 | 启动节点的 Linux/macOS 用户 |
| stdio 本地模式 | 直接提供当前机器的工具 | 启动 MCP 子进程的客户端用户 |
| connect 桥 | 将本地 stdio MCP 转发至远程 Hub | 仅持有客户端凭证 |
| TLS 入口 | HTTPS、域名、可选 Cloudflare Tunnel | 独立网络组件 |

Hub 不包含本地命令执行器。若需要管理 Hub 机器本身，必须单独注册该机器上的
节点。这样，部署在小 VPS 上的 Hub 只负责控制和转发，构建、下载、包安装等
工作负载由选中的设备承担。

## 为什么使用 Go

早期方案曾考虑 Node/TypeScript，最终实现选择 Go：

- 原兼容二进制保留；新增独立 Hub/节点角色产物，生命周期可分别管理。
- 每种角色构建 linux/amd64、linux/arm64、darwin/amd64、darwin/arm64 四种目标。
- 运行设备不必安装 Node、npm、Python 或 Electron；Python 只用于源码校验和
  发布打包，Go 只在构建时需要。
- 基于官方 MCP Go SDK 使用标准协议实现，WebSocket 用于内部 Hub-节点通道。
- 原生 POSIX 进程和 PTY 能直接继承系统用户身份，无须容器来承载设备能力。

这是部署复杂度与资源使用的工程选择，不能从语言选型直接推导最低内存。实际
RSS、空闲 CPU 和并发表现需在目标机器上测量，记录在[验证记录](validation/README.md)。

依赖版本以 go.mod/go.sum 为准。参考 TypeScript 文件保持不可修改，不进入 Go
运行时构建，也不声称其上游测试已经通过。

## 两条独立通道

外部通道为 MCP 客户端到 **https://域名/mcp** 的 Streamable HTTP；可用本地
stdio 桥接不支持直连配置的客户端。内部通道由设备主动发起 WebSocket 连接，
携带单独的设备身份，再承载工具请求和结果。

外部 MCP bearer 不是设备凭证。邀请凭证只用于加入，完成后设备使用持久凭证。
调用者允许访问的 device ID 与连接设备的身份由 Hub 判断，不由工具参数自报
身份决定。终端会话也要绑定调用主体与设备，不能只凭 session ID 访问。

Hub、节点和 MCP 协议各自有不同版本；不要将内部 WireVersion 或产品 0.2.1
当成 MCP 协议版本。已协商的协议版本和实际客户端验证记录应单独记录。

## 状态与失败语义

配置、token 与邀请都应放在仅该账户可访问的目录中，不进入 Git。Hub 配置保存
设备及访问凭证的授权状态；节点配置保存自己的连接身份。设备掉线时，后续
调用应清楚失败，不能偷偷切换到另一台机器执行。

配置 Store 每次检查都会读取当前私有文件，只在字节完全一致时复用已解析、
已校验的策略，并向调用方返回独立副本。它不使用 TTL 或仅凭文件时间戳判断
新鲜度；另一进程更新、同大小改写、权限变化或配置损坏都会在后续读取时生效。
撤销不会回滚已经派发的操作，也不能撤回已读取旧策略的在途检查。

工具结果同时提供 `structuredContent` 和序列化 JSON 的文本回退，这是
[MCP 的兼容建议](https://modelcontextprotocol.io/specification/2025-06-18/server/tools#structured-content)。
只读取文本的客户端与使用结构化字段的客户端均能得到完整结果。

网络断开不等于命令撤销。已派发命令可能已经执行，即使客户端没拿到结果。
有副作用的失败调用应先检查实际状态；不要自动重放。进程会话是设备上的运行
状态，当前版本不承诺 Hub/节点重启后恢复全部 PTY 和输出。具体保留、清理与
退出行为由运行时测试记录确认。

设备连接应支持心跳和退避重连。重连解决连接可用性，不提供分布式事务或
exactly-once 执行语义。完整的持久任务恢复、离线排队和多 Hub 高可用不属于
首版承诺。

## 权限模型

一个已注册节点对应一个物理或虚拟主机上的一个执行账户。相同机器可以运行多个
独立命名的节点，例如 `build-worker` 与 `build-root`；它们使用各自的配置、
设备凭证和服务。客户端 token 的 `devices` 列表选择这些账户节点，既能按机器
区分，也能在同一机器上按用户区分。客户端不能在工具参数里任意指定执行用户。

邀请可带 `expected_user`（CLI `device invite --user USER`），节点连接时核对
实际账户，避免管理员把预期普通用户节点误以 root 启动。这是部署身份绑定；
它不会在该账户内部创建额外的目录或命令权限策略。

设备节点使用普通系统路径、用户 UID/GID、附加组和可用的环境。用户本来
无权访问的文件会返回系统错误；其已被系统允许的文件不再经过项目目录白名单。
服务环境和交互 Shell 的 PATH 不一定相同，应通过 doctor 与实际命令确认。

Linux 的 sudo、SELinux/AppArmor、文件权限仍然有效。macOS 的 TCC、SIP、
Automation、Accessibility、屏幕录制与桌面登录状态仍然有效。节点不是通用
权限绕过机制；使用 root 启动是部署者选择的另一种身份，不是安装默认行为。

用户服务是默认部署方式。Hub 可放进容器；设备节点应原生运行，否则看到
的用户、文件系统和进程环境主要是容器中的环境。

## 组件与授权合同

私有 wire v1 与发行版本独立；新增字段为可选字段，旧请求和结果语义不变。
Hub 的 OAuth、路由和 MCP 表达升级无需替换所有节点二进制；新增原生操作需要
明确的能力协商或节点更新。详见 [组件合同](components-updates.md)。
HTTP MCP 可选外部 issuer/JWKS 验证与 RFC 9728 元数据，scope 只控制入口，
subject 的显式节点映射控制授权。legacy bearer、OAuth principal 和设备通道
彼此分离。详见 [OAuth 配置](oauth.md)。节点更新只由本机可信策略触发，Hub
没有任意二进制推送接口。

## 跨设备文件传输（transfer_file）

`transfer_file` 是 Hub 自己的工具：把一个普通文件从一台设备复制到另一台（或同一台）
设备，文件内容不经过 MCP 客户端和模型上下文。它不违反“Hub 只路由、不执行”：
Hub 不读写任何文件系统，而是以调用者的 principal 发起普通的节点工具调用——
源端 `read`（base64 + `offset`）、目标端 `write_file`（首块新建，之后 `append`
并带 `expected_size`）、最后 `move_path`，失败时用 `apply_patch` 删除暂存文件。
因此每一块都照常经过授权、撤销检查、节点能力协商和审计日志；撤销调用者对任一
设备的授权，下一块就会失败。

- **原子安装**：数据先追加到目标旁的隐藏暂存文件 `.rmc-transfer-ID`，校验通过后
  用 `move_path` 改名到位；不带 `overwrite` 时使用不替换已有文件的改名
  （Linux `renameat2(RENAME_NOREPLACE)`、macOS `renamex_np(RENAME_EXCL)`），
  所以目标不会出现半成品，也不会覆盖期间新出现的文件。
- **校验**：Hub 对读到的字节计算 sha256。16 MiB 以内的文件，源端每次 `read` 和
  目标端最后一次写入都会报告整文件 sha256，三者必须一致；更大的文件按大小校验
  （每块的 `expected_size` 保证不重写、不漏写）。源文件的大小、修改时间或
  sha256 在传输中变化即失败，不会安装。
- **后台任务**：每个 MCP 请求在 48 秒内结束，大文件可能更久，所以传输是 Hub 内存中
  属于发起 principal 的后台任务。调用最多等待 `wait_seconds`，未完成就返回
  `transfer_id`，之后凭它等待或取消，与 `exec_command`/`write_stdin` 的模式一致。
  单个 principal 最多同时 2 个、整个 Hub 最多 4 个（每个传输同时最多占用节点 1 个读和
  1 个写请求，总共不超过节点 16 个并发请求的一半）；完成的记录保留 15 分钟。
  运行中的传输让 Hub 不算空闲，自动更新会等它结束。Hub 重启会丢失任务，目标
  目录里可能留下 `.rmc-transfer-ID` 暂存文件，可直接删除。
- **吞吐**：每块 1 MiB，读下一块与写上一块并行。本机回环约 20 MB/s；跨网络时
  受带宽和往返时延限制，每块两次往返。

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
