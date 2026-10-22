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
| Codex / Claude Code | 可按其 stdio 或 header 功能配置；未记录实测前不宣称已验证 |
| ChatGPT 云端自定义 MCP 接入 | 未验证；不能假定支持任意静态 bearer header |
| OAuth-only 客户端 | 首版没有完整 OAuth 服务，不能据此声称可直连 |

最新 MCP 规范与旧版本存在握手、session 和流处理差异。项目使用官方 SDK，
实际协商版本应作为验收证据记录，不把“有 /mcp endpoint”当作所有协议版本兼容。
