# MCP 客户端接入

HostMCP 提供三种接入方式。配置格式由客户端决定；下面的 mcpServers JSON
适用于采用该结构的 stdio 客户端，其他宿主需将相同 command/args 填入其设置。
不要把一个客户端的配置文件原样当作所有客户端都接受的格式。

## 本地单机 stdio

```json
{
  "mcpServers": {
    "hostmcp-local": {
      "command": "/absolute/path/to/hostmcp",
      "args": ["stdio"]
    }
  }
}
```

此模式直接使用客户端启动进程的用户身份，不需要 Hub、设备邀请或 HTTP token。
文件路径是当前机器的真实路径；相对路径的工作目录应显式指定。

## stdio 桥接远程 Hub

```json
{
  "mcpServers": {
    "hostmcp": {
      "command": "/absolute/path/to/hostmcp",
      "args": [
        "connect",
        "--url", "https://mcp.example.com/mcp",
        "--token-file", "/absolute/path/to/client.token"
      ]
    }
  }
}
```

客户端只需能启动本地可执行文件。bridge 读取 token 文件后通过 HTTPS 连接 Hub；
不把 token 本身写进 command arguments。文件路径必须位于客户端所在机器，
而不是 Hub 的文件系统上。用绝对路径避免桌面应用 PATH 与终端 PATH 不同。

bridge 的 stdout 是 MCP 协议通道，不应在其前面添加会向 stdout 输出欢迎语的
Shell 包装。调试日志应走 stderr。此模式仍要求客户端运行 bridge 的环境能
访问远程域名。

## 直接 Streamable HTTP

在支持 remote MCP 与自定义 Authorization header 的客户端中配置：

| 项目 | 值 |
| --- | --- |
| Transport | Streamable HTTP |
| URL | https://mcp.example.com/mcp |
| Header 名 | Authorization |
| Header 值 | Bearer 后接客户端 token |

使用客户端的 secret/env 引用功能保存 token；具体语法以该客户端版本的文档为准。
不要将 token 放在 URL query，也不要改成设备邀请 token。若客户端只支持 OAuth
登录而不能设置 bearer，则首版不能直接适配其远程登录流程；有本地 stdio 能力
时可使用上面的 bridge。

## 通过内置 CLI 验证

先检查设备列表，再调用一个明确设备的身份工具：

```sh
hostmcp call --url https://mcp.example.com/mcp \
  --token-file /absolute/path/client.token \
  --tool list_devices --args '{}'

hostmcp call --url https://mcp.example.com/mcp \
  --token-file /absolute/path/client.token \
  --tool device_info --args '{"device":"ubuntu"}'

hostmcp call --url https://mcp.example.com/mcp \
  --token-file /absolute/path/client.token \
  --tool exec_command \
  --args '{"device":"ubuntu","cmd":"id","yield_time_ms":1000}'
```

远程设备工具使用 **device** 参数。本地 stdio 直接作用于本机，不需要 device。
工具返回的 session_id 是字符串；续读、输入和终止必须使用原会话所属设备与
同一个认证主体。完整输入 schema 以客户端通过 tools/list 得到的内容为准。

首版设备工具包括 device_info、exec_command、write_stdin、list_processes、
terminate_process、read、write_file、apply_patch、view_image。view_image 是
读取已有图片文件，不是屏幕捕获或桌面控制。

## 给不同客户端分配 token

在 Hub 机器上执行：

```sh
hostmcp token create --config /absolute/path/hub.json \
  --id laptop-client --devices ubuntu,mac \
  --output /absolute/path/laptop-client.token
```

为不同客户端使用不同 ID 和 token。这样可以独立撤销某个客户端，并避免把
一个 client principal 的托管终端同时交给多个不需要共享会话的客户端。

## 兼容性记录

| 接入 | 当前声明 |
| --- | --- |
| 官方 MCP SDK 与 HostMCP 内置 call | 以 validation.md 的实际测试结果为准 |
| 通用 stdio 客户端 | 已提供配置方式；具体宿主验证另行记录 |
| 自定义 bearer 的 remote MCP 客户端 | 协议目标；实际宿主和版本须分别验证 |
| Claude Code 2.1.292 | 2026-10-22 实测通过：stdio 启动 hostmcp connect，经公网 HTTPS Hub 列出 3 台在线设备，并分别查询身份；实际 4 次工具调用均成功 |
| Codex / Claude Desktop | 可按各自的 stdio 或 header 功能配置；尚未完成对应宿主验收 |
| ChatGPT 云端自定义 MCP 接入 | 未验证；不能假定支持任意静态 bearer header |
| OAuth-only 客户端 | 首版没有完整 OAuth 服务，不能据此声称可直连 |

最新 MCP 规范与旧版本存在握手、session 和流处理差异。项目使用官方 SDK，
实际协商版本应作为验收证据记录，不把“有 /mcp endpoint”当作所有协议版本兼容。

### 复现 Claude Code 隔离验收

将上方“stdio 桥接远程 Hub”的 JSON 保存为 /absolute/path/hostmcp-client.json，
替换其中的示例域名、可执行文件路径和 token 文件路径。token 正文保留在私有
文件中。使用已完成登录的 Claude Code 2.1.292；先用 claude --help 核对当前版本
支持这些参数，再在独立临时目录执行：

```sh
(
  set -eu
  hostmcp_audit_dir="$(mktemp -d "${TMPDIR:-/tmp}/hostmcp-claude.XXXXXX")"
  trap 'rmdir "$hostmcp_audit_dir" 2>/dev/null || true' EXIT
  cd "$hostmcp_audit_dir"
  claude --print \
    --strict-mcp-config --mcp-config /absolute/path/hostmcp-client.json \
    --tools '' \
    --allowedTools 'mcp__hostmcp__list_devices,mcp__hostmcp__device_info' \
    --disallowedTools \
      mcp__hostmcp__exec_command mcp__hostmcp__write_stdin \
      mcp__hostmcp__list_processes mcp__hostmcp__terminate_process \
      mcp__hostmcp__read mcp__hostmcp__write_file \
      mcp__hostmcp__apply_patch mcp__hostmcp__view_image \
    --setting-sources '' --disable-slash-commands --no-chrome \
    --no-session-persistence --permission-mode dontAsk \
    --max-budget-usd 0.5 --output-format stream-json --verbose \
    '只做只读 MCP 验收：调用 list_devices 一次，再对返回的三台在线设备分别调用 device_info 一次。仅使用这两个工具，不执行命令或访问文件。根据真实返回值总结设备 ID、OS、用户名和 UID；设备不足或调用失败时据实报告。'
)
```

验收时检查结构化事件：hostmcp 的连接状态应为 connected，且应存在一次
list_devices 和三次 device_info 的 tool_use，以及按 ID 对应的四个成功
tool_result。模型的文字总结本身不构成通过证据。上述配置禁用内置工具，并显式
拒绝其他 HostMCP 工具；不修改全局 MCP 配置，也不使用跳过权限的选项。

本次实际验收符合这些条件，permission_denials 为空、客户端退出码为 0。测试
路径使用临时 HTTPS Tunnel，结果仅覆盖这个版本的 Claude Code 与 stdio connect
组合；不推定其他 Claude Code 版本、Claude Desktop、正式 named Tunnel 或
ChatGPT 云端 OAuth 接入已完成验收。登录、计费或组织策略阻止测试时，应记录
该次未通过或未完成，不改变这些限制来取得成功结果。
