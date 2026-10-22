# HostMCP

面向 Linux 和 macOS 的无界面 MCP 主机工具项目，目标是让支持 MCP 的 agent
使用服务进程所在用户的完整文件、Shell 和进程权限。

**当前状态：源码提取基线。尚未实现可运行的独立 MCP 服务。**

本次从 Chat On Steroids `v2.1.11` 提取有复用价值的源码及回归测试，保留逐文件
SHA-256 和原始许可证。没有复制 Electron 界面、浏览器扩展、用户配置或运行数据。
当前目录中的 TypeScript 是原样参考代码，不是已经完成解耦的运行时。

## 目标

- Linux/macOS，纯命令行，可作为用户服务常驻。
- 标准 MCP：stdio 供本地客户端使用，Streamable HTTP 供远程客户端使用。
- 文件和命令使用原生系统路径，不要求在界面中批准目录。
- Shell、交互终端、后台进程和文件工具以启动该服务的 OS 用户身份执行。
- 与 agent 的模型、编排方式和浏览器无关；ChatGPT 接入也走 MCP 协议。
- 保留调用校验、输出边界、进程生命周期管理和远程接入认证。

“完整用户权限”仍受 Unix 文件权限、sudo、macOS TCC/SIP 等操作系统机制约束；
本项目不提供自动提权。客户端自身的审批和策略也不会因为接入 MCP 而消失。

## 本次提交的内容

| 目录 | 内容 |
| --- | --- |
| `reference/chat-on-steroids/src/main/codex/` | 终端会话、Shell、输出缓冲、补丁、读取、图片处理的原始实现；不含聊天归属模块 |
| `reference/chat-on-steroids/src/main/*.ts` | 文件操作、文本匹配、搜索、环境变量和进程辅助实现 |
| `reference/chat-on-steroids/src/main/mcp/` | MCP/工具封装、QuickJS 编排和附件处理的架构参考；需要重新解耦 |
| `reference/chat-on-steroids/test/` | 与提取模块相关的上游回归测试，未运行 |
| `provenance/upstream.json` | 固定上游提交、文件清单、分组和 SHA-256 |
| `scripts/` | 可复现提取脚本与不需要 npm 依赖的完整性检查 |
| `docs/extraction-plan.md` | 保留、重写、舍弃范围及下一步实现顺序 |
| `LICENSE`、`NOTICE` | 上游 MIT 授权及 Codex 派生代码的 Apache-2.0 声明 |

## 检查源码基线

```sh
python3 scripts/verify-source-import.py
```

要从固定的上游源码重新生成快照：

```sh
git clone --branch v2.1.11 --depth 1 \
  https://github.com/totec448-spec/chat-on-steroids.git /tmp/cos-source
python3 scripts/import-upstream.py /tmp/cos-source
python3 scripts/verify-source-import.py --upstream /tmp/cos-source
```

提取器只读取指定的公开 Git 源码和本仓库，不访问已安装应用或用户数据。它拒绝
不同的上游提交、脏源码以及与清单不一致的既有快照。

## 下一步

先完成原生路径与 POSIX 进程核心，再提供 stdio MCP 入口和实际回归测试；随后
加入带认证的 Streamable HTTP、远程客户端接入验证及用户级服务定义。
详细决策见[提取与实现方案](docs/extraction-plan.md)。

上游：[Chat On Steroids](https://github.com/totec448-spec/chat-on-steroids)，
固定提交：[f51acbccdd734f524799ea92bb747be765fba1e4](https://github.com/totec448-spec/chat-on-steroids/tree/f51acbccdd734f524799ea92bb747be765fba1e4)。
