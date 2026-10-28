# 账户节点与 Agent 权限

HostMCP 的权限粒度是**机器上的系统账户**。一个 `device` ID 表示一个账户节点，
不是自动包含该物理机器所有账户的入口。同机需要两个权限等级时，运行两个独立
Agent 服务，各自有邀请、配置、设备凭证和节点 ID。

| 节点 ID 示例 | 所在机器 | 实际运行账户 | 可授予的客户端示例 |
| --- | --- | --- | --- |
| workstation-worker | Linux 工作站 | worker | Codex 编码 Agent |
| workstation-root | 同一台 Linux 工作站 | root | Claude Code 运维 Agent |
| mac-developer | Mac mini | developer | Codex 或 Claude Code 开发 Agent |

这些名称只是示例。安装前先确认目标账户已经存在。获得节点访问权的客户端可以
执行该账户能执行的任何命令、读写该账户能访问的文件；没有更细的命令或目录策略。

## 1. 在 Hub 创建绑定账户的邀请

下面两条命令在 Hub 所在机器执行：

```sh
umask 077
hostmcp device invite --config /absolute/path/hub.json \
  --id workstation-worker --user worker \
  --output /absolute/path/workstation-worker-invite.json
hostmcp device invite --config /absolute/path/hub.json \
  --id workstation-root --user root \
  --output /absolute/path/workstation-root-invite.json
```

`--user` 将预期账户写入邀请和注册配置；节点必须以该账户运行才能接入。
这是防止部署身份出错的检查，不是额外的沙箱。为新部署显式设置 `--user`。
已有 v0.1.0 未绑定账户的节点仍按原配置工作；要加绑定，可撤销后重新邀请。

把两个邀请文件分别安全传到目标设备。邀请有有效期且只能兑换一次；不要将它们
粘贴到公开日志或提交到 Git。每个账户使用自己的配置文件，不能共用 device ID。

## 2. 启动普通账户节点

以 `worker` 账户登录目标机器并运行：

```sh
umask 077
mkdir -p "$HOME/.config/hostmcp"
hostmcp agent enroll --invite /absolute/path/workstation-worker-invite.json \
  --config "$HOME/.config/hostmcp/agent.json"
hostmcp agent run --config "$HOME/.config/hostmcp/agent.json"
```

先在前台确认成功，再按需要安装用户服务：

```sh
hostmcp service install --role agent \
  --config "$HOME/.config/hostmcp/agent.json" \
  --name hostmcp-workstation-worker
```

Linux 用户服务的开机/注销行为取决于该用户的 systemd manager 和 linger 设置；
Mac LaunchAgent 依赖登录会话。需要无人登录时随系统启动，可以由管理员安装
系统服务，仍然指定普通账户执行。例如 Linux 上：

```sh
sudo /usr/local/bin/hostmcp service install --system --user worker \
  --role agent --config /home/worker/.config/hostmcp/agent.json \
  --name hostmcp-workstation-worker
```

选择用户服务或系统服务中的一种，避免让同一节点的两个进程互相替换连接。
系统服务应使用管理员维护的二进制路径；二进制及其上级目录不能由低权限账户
任意改写。配置文件必须由运行账户持有并保持 `0600`，父目录应限制其他账户访问。

## 3. 启动 root 节点

root 是明确选择的第二个节点。以管理员身份注册并安装系统服务：

```sh
sudo /usr/local/bin/hostmcp agent enroll \
  --invite /absolute/path/workstation-root-invite.json \
  --config /var/lib/hostmcp/root.json
sudo /usr/local/bin/hostmcp service install --system --user root \
  --role agent --config /var/lib/hostmcp/root.json \
  --name hostmcp-workstation-root
```

Linux 使用 systemd system service，macOS 使用 LaunchDaemon。macOS 的系统
授权、SIP 和 FileVault 启动条件仍然有效。服务不会修改 sudoers 或系统隐私设置。
普通账户原先拥有的 sudo、管理员组或 Docker socket 权限仍然有效；需要没有
提权能力的节点时，应选择本来就没有这些权限的账户。

注册成功后删除对应的一次性邀请副本。通过 `list_devices` 和 `device_info`
核对 username/UID，再执行 `id -u` 确认真实子进程身份。

## 4. 给不同 Agent 发不同凭证

在 Hub 上执行：

```sh
hostmcp token create --config /absolute/path/hub.json \
  --id codex-worker --devices workstation-worker,mac-developer \
  --output /absolute/path/codex-worker.token
hostmcp token create --config /absolute/path/hub.json \
  --id claude-admin --devices workstation-root \
  --output /absolute/path/claude-admin.token
```

在对应 Agent 客户端配置自己的 token 文件，见[客户端接入](clients.md)。两个
客户端连接同一 Hub URL，但只会列出、访问各自获准的节点。一个 token 可以访问
多台机器，也可以同时访问同机多个账户；权限由 `--devices` 中的节点 ID 决定。

Hub 客户端身份与 OS 用户身份并不相同。两个不同客户端获准访问同一个账户节点
时，它们都拥有该账户的原生权限。托管 session 按客户端身份区分以避免串线；这
不构成同一系统账户内部的文件或进程隔离。

## 5. 可重复的原生权限验收

先准备同机的 root 与普通账户两个节点，以及分别授权的两个 token。运行：

```sh
python3 scripts/verify-access.py --binary /absolute/path/hostmcp \
  --url https://mcp.example.com/mcp \
  --admin-token-file /absolute/path/admin.token \
  --ordinary-token-file /absolute/path/ordinary.token \
  --root-node workstation-root --ordinary-node workstation-worker
```

脚本通过真实 MCP 调用检查：

- 普通凭证的设备列表不包含 root 节点，直接调用 root 节点也被拒绝。
- 两个节点 `device_info` 的 UID 与实际运行 `id -u` 的结果一致。
- root 在专属临时目录创建 `0600` 文件，普通节点的文件工具与 Shell 均收到原生权限拒绝。
- root 删除本次专属 fixture，脚本输出不包含 token 内容的 JSON 结果。

该脚本检查账户执行与访问范围；它不会擅自修改账户组、sudoers 或管理员权限。
完整九工具验收继续使用 `scripts/smoke.py`；长任务和重连的实际边界见
[验证记录](validation.md)。

## 6. 撤销和卸载

撤销一个客户端不会改变其他客户端的凭证；撤销一个节点不会移除该机器上的其他
账户节点：

```sh
hostmcp token list --config /absolute/path/hub.json
hostmcp token update --config /absolute/path/hub.json \
  --id codex-worker --devices workstation-worker
hostmcp token rotate --config /absolute/path/hub.json \
  --id codex-worker --output /absolute/path/codex-worker-new.token
hostmcp token revoke --config /absolute/path/hub.json --id codex-worker
hostmcp device revoke --config /absolute/path/hub.json --id workstation-root
sudo /usr/local/bin/hostmcp service uninstall --system --name hostmcp-workstation-root
```

撤销阻止后续授权操作，不回滚已产生的系统效果。需要停止已运行任务时，应在撤销
前终止它们或通过设备管理渠道处理。服务卸载保留配置文件，便于重新安装；确认不
再需要时由所有者清理相应配置和二进制。

`token update` 保留凭证而改变授权节点；`token rotate` 保留客户端身份和范围，
立即令旧凭证失效。轮换时需要把新 token 文件分发到对应 Agent。
