# 安全模型与权限

## 明确的信任关系

设备所有者主动安装并启动 Agent，将这个账户的命令和文件能力交给所连接的
Hub。拥有相应客户端 token 的调用方，可以对获准设备执行该用户能执行的操作。
这包含调用其现有工具、读取其可读文件、修改其可写文件，以及使用已有 SSH
配置等。客户端自己的审批策略仍然生效。

授权的最小单位是**机器上的一个系统账户节点**。相同机器的普通用户节点与
root 节点分别注册、分别运行、使用不同设备凭证；客户端 token 按节点 ID 授权。
邀请的 `--user` 绑定预期执行账户，防止配置被无意用于另一个账户。项目不在
账户权限之下再加目录白名单、命令过滤或工具级角色。

账户本身已有的 `sudo`、Docker socket、管理员组等能力也属于它的原生权限。
如需要一个实际无管理员权限的节点，请使用系统上本来就没有这些权限的专用账户。
把已有管理员账户称为“普通节点”并不会收回它已有的能力。root 节点的权限范围
由系统本身决定，不能用同机另一个普通节点保护该机器上的秘密免受 root 访问。

同一个 Hub 应属于一个信任域。首版不提供面向陌生租户的 SaaS 隔离。具有命令
执行权的客户端能访问目标用户自己的配置；按主体隔离托管 session 是防止串线
的协议约束，不应宣传成同一 OS 用户内部的强隔离沙盒。

## 凭证类型

| 凭证 | 用途 | 保存位置 |
| --- | --- | --- |
| 客户端 bearer | 调用 Hub 的 MCP 工具 | 客户端 token 文件 |
| 单次设备邀请 | 限时加入指定 device ID | 临时邀请文件 |
| 设备凭证 | Agent 连接 Hub | Agent 配置文件 |
| Cloudflare Tunnel 凭证 | 发布 Hub 网络入口 | cloudflared 配置目录 |

这些凭证不可互相替代。Hub 需要验证每次 MCP 请求；可猜到的 device ID 或
session ID 都不是授权依据。配置与 token 文件使用 0600，所在目录使用 0700。
不要把真实配置、邀请、日志中的秘密或本机连接信息写进示例、测试 fixture、
命令行输出截图或 Git。

## 撤销与轮换

```sh
hostmcp token list --config /absolute/path/hub.json
hostmcp token update --config /absolute/path/hub.json \
  --id client --devices workstation-worker,mac-developer
hostmcp token rotate --config /absolute/path/hub.json \
  --id client --output /absolute/path/client-new.token
hostmcp device revoke --config /absolute/path/hub.json --id ubuntu
hostmcp token revoke --config /absolute/path/hub.json --id client
```

只需访问部分设备的客户端，应把 --devices 设置为所需 ID 列表，避免通配符。
`token list` 只列出身份与节点范围，不输出 token 或 hash；`update` 保持现有
凭证并更新节点范围；`rotate` 保持客户端身份与范围，原子生成替代凭证，旧凭证
随即失效。轮换后将新文件安全分发到客户端。需要重叠迁移窗口时，先为独立 ID
创建新 token，客户端切换后再撤销旧 ID。设备撤销、活跃连接处理和配置重新加载的
具体测试结果见 validation.md。撤销授权不代表已完成的文件操作回滚，也不代表
外部程序已产生的效果被撤销。

从 v0.2.1 起，每个客户端凭证拥有独立的内部进程会话身份；即使撤销后再次创建
相同的客户端 ID，新的凭证也不能继承旧凭证启动的托管进程与去重记录。Hub 将
认证失败（采样限速）、节点上下线、工具调用的客户端 ID/设备 ID/工具名/请求 ID/
结果写入服务日志，**不会记录 token、工具参数或文件内容**。这些日志用于故障
排查，不是不可篡改的企业级审计系统；管理员仍应保护日志权限和设置保留期。

## 网络入口

原生 Hub 默认监听回环地址；使用 Cloudflare named Tunnel 或 HTTPS 反向代理
公开入口。设备通过 TLS 访问公开地址。Origin 校验与 bearer 验证是不同层面的
检查，应保留两者。发布者应将 public-url 配为真实域名，不能通过任意请求的
Host/X-Forwarded-Host 反推出可信身份。

Cloudflare Tunnel 提供网络路径与 TLS，不自动提供 HostMCP 的用户授权。
首版支持的默认组合是 named Tunnel 加 HostMCP bearer。内置 agent、connect
和 agent enroll 尚不能配置 Cloudflare Access 的服务凭证 headers，因此要求
Access 的域名策略会阻断这些组件；外部客户端能附加 headers 也不能单独解决
Agent 连接与设备加入。只有整条链路的服务凭证传递已经另外实现并验证后，
才可添加 Access 这一层；邮箱登录页不适用于这些自动化连接。

首版使用静态 bearer，不实现完整 OAuth 2.1 discovery、authorization code、
PKCE、刷新 token 或多用户登录。不要把其认证配置当作所有 SaaS MCP 宿主必然
支持的方案。

## 完整用户权限的边界

- 不限制在某个仓库或批准目录；符号链接按原生文件系统语义处理。
- 不自动编辑 sudoers、授予 root、修改系统隐私数据库或关闭系统保护。
- 用户服务的环境与终端环境可能不同，尤其是 PATH、SSH agent 和桌面 session。
- macOS 的完整磁盘访问等授权应交给系统设置；headless 服务不能凭 UID 假定
  自动拥有所有 GUI 能力。
- 输出、文件大小、并发和 session 数量的上限是资源边界，不是目录权限白名单。
- 同一客户端凭证最多同时占用 4 个远端 MCP HTTP 请求，全局容量为 24；
  设备邀请加入有独立的 2 个槽位。满载时短暂排队并返回 503，调用方须退避重试。
- `apply_patch` 更新现有文件时保留其 inode、所有者及权限位；但多文件补丁
  **不是跨文件原子事务**，写入失败时可能需要人工检查部分成功的操作。
- shell 的普通后台进程随托管命令的进程组结束而退出，Agent 进程重启会丢失
  托管会话。需要跨服务重启存活的任务，应使用独立的 systemd/launchd 服务。
- 执行器返回的文件内容和命令输出是不可信数据，MCP 客户端应按数据处理，不能
  将其中的指令当成新的用户授权。

## 问题报告

报告问题时提供版本、系统、脱敏 doctor 信息和最小复现。不要在公开 issue 中
附带 live token、完整配置或真实用户文件。若要报告可能暴露远程执行权限的漏洞，
优先使用仓库开启的 GitHub 私密安全报告功能；未开启时先用不含利用细节和秘密
的 issue 联系维护者，不在公共讨论中贴实际凭证。

## 依据

- [MCP 授权](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization)
- [MCP 安全实践](https://modelcontextprotocol.io/docs/2026-07-28/tutorials/security/security_best_practices)
- [Cloudflare Service Tokens](https://developers.cloudflare.com/cloudflare-one/access-controls/service-credentials/service-tokens/)
- [Apple 文件访问授权](https://support.apple.com/guide/security/controlling-app-access-to-files-secddd1d86a6/web)
