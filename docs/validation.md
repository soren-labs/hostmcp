# 验证记录

此文件区分已执行结果与验证计划。所有测试使用 HostMCP 自己的隔离临时 fixture，
不接入、修改或重启正在运行的 Chat On Steroids 安装及其用户数据。

## 当前证据

初始源码基线有 62 个参考文件，固定上游 v2.1.11；完整性由
scripts/verify-source-import.py 校验。参考代码保存不代表运行时已经存在，也
不代表上游 TypeScript 测试已经执行。

**下面的运行时与真实部署结果等待执行者回填。pending 不代表通过。**

| 检查 | 状态 | 证据 |
| --- | --- | --- |
| 参考源码 SHA-256 与许可 | passed | 2026-10-22：62 个文件、722,204 bytes，固定上游 f51acbccdd734f524799ea92bb747be765fba1e4 全部一致 |
| 安装/发布脚本语法 | passed | 2026-10-22：sh -n scripts/install.sh、bash -n scripts/release.sh |
| 配置模板语法 | passed | 2026-10-22：四个 YAML 解析成功，两个 launchd plist 通过 plutil；不等于 Docker/Caddy/隧道部署成功 |
| 安装器失败行为 | passed | 2026-10-22：隔离 fixture 验证正确校验安装、篡改/重复/缺失 SHA 拒绝、符号链接 binary 拒绝、失败时保留原 binary |
| go vet ./... | pending | 等待运行时完成 |
| macOS go test -race ./... | pending | 等待原生测试 |
| Linux go test -race ./... | pending | 等待原生或 CI 测试 |
| 四平台交叉构建 | passed | 2026-10-22：scripts/release.sh v0.1.0 成功生成 linux/darwin × amd64/arm64；本地开发构建，尚非发布证据 |
| 校验和与本地 archive 安装 | pending | 等待产物生成 |
| 本地 stdio MCP | pending | 等待真实 MCP 调用 |
| HTTP MCP 协商与工具调用 | pending | 需记录 SDK/协议版本 |
| Ubuntu、Mac、VPS 三设备调用 | pending | 需记录目标系统、身份与结果 |
| 公网 named Tunnel / HTTPS | pending | 需从外部网络验证 |
| 认证失败、撤销与权限范围 | pending | 等待负例验收 |
| 设备重连 | pending | 只断开测试 Agent，不改宿主网络 |
| 用户服务生命周期 | pending | 需分别验证 systemd 与 LaunchAgent |
| Docker Hub 模板 | pending | 尚不能声称已完成 Docker 验收 |
| 低配 VPS 资源使用 | pending | 记录机器内存与进程 RSS |
| Codex / Claude Code 宿主接入 | pending | 通用配置不是宿主实测 |
| ChatGPT 云端接入 | pending | 首版无 OAuth，不能默认宣称兼容 |

## 三设备端到端验收

每个设备应分别验证：身份信息与本机 id 一致、临时目录文件写读一致、命令输出
属于正确设备、长任务返回并能继续查询 session、终止只影响该托管进程组。
测试凭证应专门生成，测试后撤销。不要把生产 secret、IP、域名、邀请或用户文件
内容写进公开报告。

网络测试覆盖：无凭证、错误凭证、非法 Origin、无权访问的 device ID、撤销后的
请求、Agent 离线与恢复。重连不能自动重放已可能执行的副作用命令。

测试跨越 HTTP 客户端、Hub、设备和实际 OS 结果才算端到端。只在 Hub 上返回一个
模拟结果、只跑单元测试，或只创建配置文件，都不能代替三设备实际调用。

## 资源测量方法

记录操作系统、架构、CPU 数、机器内存、HostMCP 版本、设备连接数与并发任务数。
分别测量 Hub、Agent、cloudflared 的进程 RSS；运行中命令的内存单独计算。
至少比较空闲、三个设备连接、并发输出负载。不要将某台机器的空闲 RSS 推广为
任意部署的最低配置保证。

示例只查询已知 PID：

```sh
ps -o pid=,rss=,pcpu=,comm= -p HOSTMCP_PID
```

RSS 单位通常为 KiB，实际格式以系统 ps 为准。峰值、长期泄漏与压力稳定性需要
专门持续测试，首次功能验收不应声称已经完成长期可靠性验证。

## 当前版本的明确限制

- 静态 bearer；没有完整 OAuth discovery/login/refresh。
- 一个信任域；没有陌生租户隔离、计费或 SaaS 账户系统。
- 不承诺 Hub/Agent 重启后恢复所有 PTY、进程会话和未读取输出。
- 网络重试不是 exactly-once 执行；失败调用可能已经产生效果。
- 没有 GUI 键鼠或屏幕捕获；view_image 只读取现有图片。
- macOS 同 UID 不等于绕过 TCC/SIP；Linux/macOS 保留系统权限错误。
- 不承诺所有 MCP 版本和所有客户端；以具体测试组合为准。
- 提供 CI/release workflow 不等于远端 workflow 已运行成功，也不等于 release 已发布。
