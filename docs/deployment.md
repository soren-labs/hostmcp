# 部署指南

## 选择拓扑

| 环境 | 推荐组合 | 外网入口 |
| --- | --- | --- |
| 小 VPS | Hub 原生服务或 Hub 容器；各设备原生 Agent | Caddy HTTPS 或 Cloudflare named Tunnel |
| 家庭 Ubuntu | Hub 用户服务与 cloudflared；各设备原生 Agent | Cloudflare named Tunnel |
| 家庭 Mac | Hub LaunchAgent 与 cloudflared；各设备原生 Agent | Cloudflare named Tunnel |
| 单机使用 | hostmcp stdio | 不需要公开入口 |

所有 device Agent 都主动连接 Hub。设备无需公网 IP、SSH server 或路由器端口
转发。Hub 的公开地址仍必须能从各设备及 MCP 客户端访问。

首版的默认目标是一个所有者管理自己的设备。完整安装步骤由 README 的 Hub
init、device invite、agent enroll/run 组成，本页补充网络与常驻服务细节。

## 原生安装

从源码构建或安装真实存在的 Release 后，将 hostmcp 放到用户可写的目录，例如
~/.local/bin。Hub 与 Agent 使用独立的配置文件。推荐布局：

| 文件 | 用途 |
| --- | --- |
| ~/.config/hostmcp/hub.json | Hub 配置与授权状态 |
| ~/.config/hostmcp/owner.token | 所有者 MCP token |
| ~/.config/hostmcp/agent.json | 本机 Agent 的连接配置 |
| 临时目录中的 invite.json | 一次性设备邀请，使用后删除副本 |

目录设置 0700，秘密文件设置 0600。不要把部署状态放进 Git checkout。通过
hostmcp doctor 和 device_info 确认运行账户、主目录与实际环境。

Hub 若以独立服务账户运行，本机 `hub configure`、`device invite/revoke` 和
`token create/update/rotate/revoke` 等管理命令也应使用该账户。例如：

```sh
sudo -u hostmcp /usr/local/bin/hostmcp device invite \
  --config /var/lib/hostmcp/hub.json --id workstation-worker --user worker \
  --output /var/lib/hostmcp/workstation-worker-invite.json
```

这样配置、锁文件和生成的邀请始终由 Hub 账户持有，配置目录也能保持正确权限。

## Cloudflare named Tunnel：VPS 与家庭设备通用

需要 Cloudflare 账户、用于发布应用的域名，以及能出站联网的 Hub 机器。域名
必须按 Cloudflare 的要求完成接入。隧道无需开放 Hub 的入站端口；本地服务
可以继续监听 127.0.0.1:8787。

1. 在 Cloudflare 中创建 named Tunnel，安装 cloudflared。
2. 将域名（例如 mcp.example.com）指向该 tunnel。
3. 将该域名的服务地址配置为 http://127.0.0.1:8787。
4. 将 Hub 的 public-url 初始化为 https://mcp.example.com。
5. 在独立网络上运行 hostmcp call，验证真实 HTTPS 入口。
6. 配置 cloudflared 自身的服务启动；它与 HostMCP 是两个独立进程。

如果先用回环地址测试、之后才确定域名，可修改已有 Hub 配置：

```sh
hostmcp hub configure --config "$HOME/.config/hostmcp/hub.json" \
  --public-url https://mcp.example.com
```

监听地址也可以通过 --listen 修改；改变监听地址需要重启 Hub 进程。修改公开
地址后再生成邀请，已加入设备的配置仍保存旧 URL，需要相应更新或重新邀请。
额外 Host/Origin 仅通过 --allow-hosts/--allow-origins 显式添加，避免通配整个
互联网。--allow-insecure 仅供明确选择的可信私网测试，正式入口使用 HTTPS。

仓库提供 [cloudflared.yml](../deploy/cloudflared.yml) 的本地管理配置样例。
替换 tunnel UUID、credentials-file 和 hostname 后再使用；该文件不包含真实
账户信息。也可以在 Cloudflare dashboard 使用 remotely-managed tunnel，
按照控制台给出的 service 命令配置。

不要将 Cloudflare 的管理 API token 或 tunnel credential 当成 HostMCP bearer。
首版的默认部署是 named Tunnel 加 HostMCP 自身的 bearer 验证。内置的 agent、
connect 和 agent enroll 尚不支持配置 Cloudflare Access 所需的服务凭证 headers。
因此不要直接在整个 HostMCP 域名上开启要求 Access 服务凭证的策略，否则设备
加入、WebSocket 连接和内置客户端都会被拦截。某个外部 MCP 客户端能够添加
Access headers，不代表设备到 Hub 的整条链路已经兼容。邮箱登录页同样无法
由这些自动化组件完成；需要 Access 的部署必须先提供并验证整条链路所需的
凭证传递方案，首版不将其作为已支持的部署方式。

### Quick Tunnel 的用途

cloudflared tunnel --url 可以产生临时地址，但官方明确没有 uptime 保证、域名
会变化，并且不支持 SSE。可以用它演示明确使用 JSON 短响应的路径，不能以
这种演示替代正式 named Tunnel 和真实 MCP 客户端验收。

## 自有域名与直接 HTTPS

如果 VPS 有公网地址，可将域名 DNS 指向它，使用 Caddy 终止 HTTPS并反代到
127.0.0.1:8787。示例见 [Caddyfile](../deploy/Caddyfile)：

```sh
export HOSTMCP_DOMAIN=mcp.example.com
caddy run --config ./deploy/Caddyfile
```

该方式需要域名解析、外部可达的 TLS 端口，以及满足 Caddy 证书获取方式的网络
条件。Caddy 的安装和常驻服务管理使用其官方流程。HostMCP 不自动修改现有
DNS、防火墙或占用中的反向代理配置。

WebSocket Upgrade 必须能正常转发；不能把设备连接路径当成普通静态网页缓存。
大命令要通过 HostMCP 的 session 查询输出，避免一个 HTTP 请求一直等到编译
结束。Cloudflare 边缘更新或网络波动都可能断开 WebSocket，Agent 需要重连。
连接恢复不保证重新派发一条命令安全，失败后先检查 session 和实际工作状态。

## Linux 用户服务

优先使用 CLI 生成与本机路径一致的服务：

```sh
hostmcp service install --role hub \
  --config "$HOME/.config/hostmcp/hub.json" --name hostmcp-hub
hostmcp service install --role agent \
  --config "$HOME/.config/hostmcp/agent.json" --name hostmcp-agent
```

只安装这台机器需要的角色。Hub 不会自动成为可执行设备。
仓库的 [systemd 示例](../deploy/systemd/)用于手工部署；它们是用户级 unit，
用户服务使用它们；系统服务使用下方 `--system` 方式生成。

```sh
systemctl --user status hostmcp-hub
journalctl --user -u hostmcp-hub -n 100
systemctl --user restart hostmcp-agent
```

如果需要用户注销后仍运行、并在开机时启动用户 manager，管理员可为该用户启用
loginctl enable-linger。是否已启用可以用 loginctl show-user USER -p Linger
检查；不要将“启用 user unit”等同于“机器未登录时一定会启动”。系统没有可用
user bus 时，先在该用户的正常登录环境配置，或由管理员部署指定 User= 的系统
unit。安装器不会更改 sudoers。

Agent unit 不添加限制其用户原有文件能力的 ProtectHome/动态用户策略，也不
额外禁止用户本来可用的提权路径。Hub 可以采用更严格的服务隔离，因为它不
执行设备命令。

## macOS 用户服务

相同 service install 命令生成用户 LaunchAgent。样例见
[launchd 模板](../deploy/launchd/)。plist 中的路径必须是真实绝对路径；launchd
不会替换其中的 $HOME 或波浪号。使用模板前创建日志目录并替换 /Users/YOU。

CLI 生成的 label 为 com.hostmcp.hostmcp-hub 或 com.hostmcp.hostmcp-agent
（自定义 --name 时替换最后一段）。可使用 launchctl print gui/UID/LABEL
检查状态。仓库中的 io.hostmcp.* plist 是手工安装示例，label 与 CLI 生成物不同；
手工安装的服务应使用 launchctl bootout 和删除该 plist 的方式移除。

LaunchAgent 与用户登录会话关联。不要将它描述成在 FileVault 尚未解锁、用户
未登录时仍保证可用的系统 daemon。设备睡眠、关机或断网时，Hub 应显示离线。

macOS 文件访问仍受 TCC 等机制控制。Agent 运行于当前 UID 不能自动获得完整
磁盘访问、屏幕录制或 Accessibility；首版也没有键鼠或屏幕捕获工具。需要的
文件访问授权由用户通过系统设置授予对应进程，系统拒绝应原样返回。

## Linux / macOS 系统服务与多账户

v0.2.1 可由管理员安装 systemd system service 或 macOS LaunchDaemon。执行
账户由 `--user` 显式指定，root 也使用相同机制。每个账户节点使用独立的服务名
和配置文件，完整注册过程见[账户节点与权限配置](account-nodes.md)。

```sh
sudo /usr/local/bin/hostmcp service install --system --user worker \
  --role agent --config /home/worker/.config/hostmcp/agent.json \
  --name hostmcp-worker
sudo /usr/local/bin/hostmcp service install --system --user root \
  --role agent --config /var/lib/hostmcp/root.json --name hostmcp-root
```

macOS 将示例中的普通用户名称和主目录换成实际账户；系统服务不要使用仅该用户
可改写的开发构建文件。普通用户服务继续使用原命令，不需要 `--system`。
同一个节点只能选择一种服务方式，避免重复进程争用同一 device ID。

系统服务配置必须由执行账户持有、权限为 `0600`，且该账户能够穿越父目录并
读取文件；Hub 还要能写入配置目录以更新注册和凭证。root 服务的二进制、配置
及其父目录必须由 root 持有，且不能由 group/other 写入。安装器会检查这些条件。
服务安装拒绝覆盖已存在的 unit；升级时在原路径替换二进制，再重启已有服务。

Linux 系统服务使用 `systemctl status NAME`、`journalctl -u NAME` 检查；macOS
系统服务使用 `launchctl print system/com.hostmcp.NAME` 检查。系统服务的卸载
也明确传入 `--system`，不会自动删除同名用户服务或配置文件：

```sh
sudo /usr/local/bin/hostmcp service uninstall --system --name hostmcp-root
```

macOS 系统服务日志位于 `/Library/Logs/HostMCP/NAME/service.log`，用户服务日志
位于 `~/Library/Logs/HostMCP/NAME.log`。卸载可重复执行，保留配置和日志；它不
代替在 Hub 上撤销设备凭证。

## 容器部署 Hub

容器仅用于 Hub。设备 Agent 在各自宿主系统中原生运行，才能看到该用户实际的
文件、工具与进程环境。

```sh
docker compose -f deploy/compose.yml build
docker compose -f deploy/compose.yml run --rm hub \
  hub init --config /state/hub.json --listen 0.0.0.0:8787 \
  --public-url https://mcp.example.com --client-token-file /state/owner.token
docker compose -f deploy/compose.yml up -d hub
```

容器内部监听所有接口是为了宿主端口映射；compose 仅将端口发布到宿主回环地址。
通过同一 compose 项目的 run 命令执行 device invite、token create 等配置操作，
使用 /state/hub.json。敏感输出文件存于同一个 named volume；需传出邀请时，
使用 docker cp 等受控传输方式，避免将秘密打印到公开日志。

停止并保留状态：

```sh
docker compose -f deploy/compose.yml down
```

不要加 --volumes，除非明确决定删除配置、邀请和凭证。容器示例是否实际构建与
验证以 validation.md 为准，提供模板不等同于已经通过 Docker 实机验收。

## 独立组件、OAuth 与可选更新

新源码的 `hostmcp-hub` 与 `hostmcp-agent` 有独立产物和安装路径；兼容二进制
`hostmcp` 保留。Hub 认证、元数据和路由升级保持 wire v1 时，无需重新部署每个
Agent。新原生工具或执行器修复仍需要 Agent 更新。版本合同、健康字段、批次
升级、离线归档校验、回滚与 systemd/launchd 重启命令见
[组件兼容与更新](components-updates.md)。该文档区分已运行版本与磁盘上的新文件。

远程 OAuth 通过外部 provider 配置，详细步骤见 [OAuth](oauth.md)。只重启 Hub；
Agent 不接受 OAuth 客户端凭证，也无需重新注册。反向代理必须公开受保护资源
元数据路径并透传 `/mcp` 的 challenge 与 Authorization header。

可选 Agent 更新通过每节点本地策略启用，固定 stable 版本和平台归档 SHA-256，
不接受 Hub URL 下发、不默认更新 root、不提升权限或自动重启。可由用户 timer
定时调用，再在维护窗口重启并检查在线版本；没有物理开机或故障硬件修复功能。

## 升级、撤销与卸载

先备份受保护的配置，再安装目标版本二进制，重启该角色服务并执行 list_devices
和 device_info 验证。按[兼容合同](components-updates.md)与 release notes 升级，不假定任意
未来 wire/config 版本都兼容。活动终端可能受到 Agent 重启影响，应先结束或另行管理重要任务。

撤销设备和 token 的命令见 [security.md](security.md)。卸载用户服务：

```sh
hostmcp service uninstall --name hostmcp-agent
hostmcp service uninstall --name hostmcp-hub
```

服务卸载与删除二进制、配置文件是不同操作。确认不再需要后，用户可移除自己
创建的 HostMCP 配置和可执行文件。若还使用 Cloudflare Tunnel，独立停止其服务
并清理对应发布配置；不要删除其他应用共享的 tunnel 或 DNS 记录。

## 参考

- [Cloudflare Tunnel 设置](https://developers.cloudflare.com/tunnel/get-started/)
- [Quick Tunnel 限制](https://developers.cloudflare.com/tunnel/get-started/quick-tunnels/)
- [Cloudflare WebSocket](https://developers.cloudflare.com/network/websockets/)
- [Cloudflare 524 与长任务](https://developers.cloudflare.com/support/troubleshooting/http-status-codes/cloudflare-5xx-errors/error-524/)
- [systemd loginctl](https://www.freedesktop.org/software/systemd/man/latest/loginctl.html)
- [Caddy reverse_proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)
