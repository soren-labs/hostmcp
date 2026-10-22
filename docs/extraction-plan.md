# 源码提取与无界面改造方案

## 基线和范围

本仓库先建立可审计的源码基线，尚未提供可运行的 MCP 服务。本次没有修改上游
源码，也没有安装依赖、执行上游测试或启动任何应用。全部参考文件保持原字节，
文件清单和 SHA-256 在 `provenance/upstream.json`。

基线是公开标签 `v2.1.11` 对应的提交
`f51acbccdd734f524799ea92bb747be765fba1e4`，不是跟随变化的 main。
所检查安装包也报告 2.1.11，六个随包扩展 JavaScript 文件与标签源码一致，但
后端存在差异，例如终端会话容量和 MCP 路径持久化实现不同。因此本仓库只声称
固定于该公开源码提交，不声称重建安装包的全部字节，也不发布反编译包或运行数据。

## 保留、重写、舍弃

| 上游模块 | 决策 | 原因 / 后续工作 |
| --- | --- | --- |
| `codex/unified-exec*`、`head-tail-buffer`、`truncate` | 复用核心 | 交互输入、异步输出、后台会话和有界缓冲；保留进程树清理 |
| `codex/shell`、`command-batch`、`exec`、`env` | 提取 POSIX 部分 | Linux/macOS 保留普通用户环境；后续移除 Windows 分支及打包资源路径假设 |
| `codex/apply-patch/*` | 复用补丁核心 | 保留语法、匹配、换行与部分失败信息；授权层不沿用 approved roots |
| `codex/filesystem`、`read-backend`、`fsops`、`text-match`、`diffstat` | 复用文件核心 | 改为真实绝对路径和显式 cwd；不限制在用户选中的目录内 |
| `search`、`ripgrep` | 精简复用 | 优先 PATH 中的 rg，提供明确的工具缺失错误；移除 Electron 打包资源依赖 |
| `codex/view-image` | 可选能力 | 仍有图片字节和解码内存上限；适合支持 MCP 图片结果的客户端 |
| `mcp/server`、`tools`、`tools-core`、`kernel` | 参考后重写 | 现有封装依赖配置、会话、浏览器归属和桌面应用生命周期，不能直接作为独立服务 |
| `mcp/inbound`、`call-context`、`tool-declarations` | 精简设计 | 标准 MCP 请求/连接上下文替代 ChatGPT 网页 conversation ID |
| `mcp/code-mode-*` | 可选移植 | 去掉聊天身份证据、Windows sky 和记录器依赖；保留 QuickJS 资源边界及已派发操作语义 |
| `mcp/artifact-*` | 仅参考 | ChatGPT 原生附件参数不是通用客户端契约；首版不暴露专用附件下载工具 |
| 相关上游测试 | 保存参考 | 在适配后的模块上运行；当前未宣称这些测试已通过 |
| `sandbox`、approved roots、能力设置界面 | 不引入 | 目标明确为服务 OS 用户的完整普通权限，不叠加目录授权流程 |
| `codex/ownership`、`session`、`workspace`、`projects` | 不引入 | 用 MCP 连接/认证主体管理终端所有权，移除浏览器聊天及项目侧栏状态 |
| `agents`、`goal`、`bridge`、浏览器扩展 | 不引入 | 模型和多代理编排由调用方负责，服务不控制 ChatGPT 网页 |
| `renderer`、`preload`、`ipc`、tray、自动更新、安装器 | 不引入 | 服务完全无界面，不需要 Electron |
| native desktop/键鼠/屏幕辅助程序 | 不引入 | 首版专注文件、命令与进程；没有桌面自动化需求 |
| Electron 构建、旧 release 工作流、上游 lockfile | 不引入 | 在新运行时依赖确定后重新生成清单和 lockfile |
| 用户配置、凭据、隧道标识、会话、日志、安装包 | 不提交 | 这些属于本机部署数据，不是公开项目源码 |

上游目录中的 Windows 分支、Electron 兼容分支和旧导入路径目前仅为保持参考
源码哈希而原样存在，不能据此宣称新服务依赖它们或支持 Windows。

## 目标运行时

```text
MCP client
  -> stdio / authenticated Streamable HTTP
  -> schema validation + connection/principal context
  -> native paths / POSIX process-session manager / patch engine
  -> operating-system user permissions
```

不增加逐目录批准或聊天身份检测。相对路径以显式 cwd 或服务启动 cwd 解析；
绝对路径按正常 OS 权限处理。符号链接采用普通系统语义。无权限、只读文件系统、
sudo 需求、TCC/SIP 等错误直接返回，不伪造成功，也不自动提权。

完整用户权限与远程接入认证是两回事。stdio 由启动它的 MCP 客户端控制；HTTP
必须有可识别的认证主体和按主体归属的进程会话。输出截断、参数大小和并发上限
用于资源管理，不是文件权限边界。取消工具调用或断开网络不意味着已经派发的
命令回滚；进程状态与终端会话需要能独立查询和终止。

## 首版候选工具

| 工具 | 行为 |
| --- | --- |
| `exec_command` | 普通用户 Shell，显式 cwd/env，返回输出或可续接的 session ID |
| `write_stdin` | 继续已有终端、输入文本、读取增量输出、发送中断 |
| `list_processes` | 查看当前认证主体所属的托管终端，区分运行和退出但输出未读 |
| `terminate_process` | 显式停止托管进程树，保留真实退出状态 |
| `read` | 查看真实路径的目录、元数据或有界文本 |
| `apply_patch` | 对真实路径增删改移文件，返回实际改动和部分失败 |
| `view_image` | 可选的 MCP 图片结果 |
| `exec` | 可选的有界 JavaScript 工具编排，避免成为首版必需依赖 |

普通命令已经可以使用网络、SSH、包管理器和用户现有工具；首版不再分别添加
SSH、下载、系统管理等重复的专用 API。服务运行于 Mac 或 Linux，不会因此自动
得到另一台机器的身份或权限。

## 客户端兼容性

本地客户端优先使用 stdio；远程客户端使用 Streamable HTTP。ChatGPT 的远程
接入使用可达的 HTTPS 服务或其支持的私有隧道；不假设所有宿主都能直连 localhost。
不假设任意客户端都支持相同认证或图片/附件参数。HTTP 认证需要单独验证 ChatGPT
支持的 OAuth 发现和授权流程，不能把一般客户端的任意 API-key header 配置视为
ChatGPT 自定义插件必然支持的方式。

依据为 2026-10-22 检查的官方文档：

- [MCP server](https://developers.openai.com/plugins/concepts/mcp-server)
- [Connect and test](https://developers.openai.com/plugins/deploy/connect-chatgpt)
- [Authentication](https://developers.openai.com/plugins/build/auth)
- [Local MCP configuration](https://learn.chatgpt.com/docs/extend/mcp)

## 实现顺序与验收

1. 建立独立 Node/TypeScript 包，原生路径与 POSIX 进程管理，删除 Electron 耦合。
2. stdio 入口、工具清单、文件/命令/终端完整调用；日志仅写 stderr。
3. 在隔离临时目录验证文件修改、cwd/env、交互 TTY、进程树终止、有界输出、
   并发调用和退出后输出回收。验证路径不受任意项目目录限制。
4. 加入 Streamable HTTP、认证主体隔离、会话管理与正常关闭；补充协议集成测试。
5. 在 Linux 和 macOS 实机或 CI 分别运行；提供用户级 systemd / launchd 示例。
6. 验证真实 MCP 客户端和 ChatGPT。通过之前不声称已支持全部客户端或平台。

## 本次实际检查

已核对固定标签及提交、上游工作树清洁状态、安装包扩展源码一致性、模块依赖和
许可证，并运行快照完整性检查。CI 只验证快照和 Python 脚本；没有运行服务、
没有执行上游 TypeScript 测试，也没有改动正在运行的 Chat On Steroids。
