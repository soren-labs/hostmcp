# HostMCP v0.2.1 修复验收

本版本是针对两份独立 v0.2.0 审查的修复版本。两份原始审查分别由 Claude
Opus 5.5 xhigh 和 Codex GPT-6.1 Sol xhigh 执行，结论与缺陷的复现依据不应
被误解为 v0.2.1 的自动验收。以下是修复工作树上**重新执行**的检查。

## 代码与本地回归

- 62 个固定来源文件、722,204 字节保持原样；`verify-source-import.py` 通过。
- macOS arm64 / Go 1.27.3：`go vet ./...`、
  `go test -race -count=1 -timeout 180s ./...` 全部通过。
- 新增测试覆盖原生符号链接/`..` 路径、原位补丁与硬链接/权限、失败恢复、
  root 编辑非 root 所有文件、终端 stdin 阻塞、进程生命周期、客户端限流及
  enrollment 隔离、配置读错误、凭证换代身份、鉴权失败退避、systemd 字符转义。
- 真实 stdio MCP 九工具脚本 `scripts/smoke.py --mode stdio` 通过。
- `python3 -I scripts/verify-access.py --help` 通过，消除了相对导入依赖。

## Linux 真实系统复核

- Ubuntu-dev02 amd64：将本版本 Go host 测试交叉编译后送入用户账户执行，
  原生路径、增删补丁、stdin 阻塞及进程生命周期相关测试全部通过。
- 同一台 Ubuntu 的实际 **root UID 0** 执行两项文件补丁测试，检查 root
  编辑 UID 65534 所有的文件仍保留 UID/GID 65534 和 `0664` 权限，均通过。
- 在 Ubuntu-dev02 创建专用临时 systemd **system service**，使用真实文件名
  包含字面量 `${HOME}` 的 Agent 配置：启动成功，服务日志证明正确读取配置
  并尝试连接回环测试地址，而非把字面量展开成环境变量。该服务及临时二进制和
  配置在测试后被清除。现有 worker/root 服务仍为 active。

## 范围和限制

- 每个客户端凭证最多 4 个并行 MCP 请求，全局默认 24 个请求；设备加入与
  长时工具调用有独立的准入通道。测试未证明无限规模的多租户隔离。
- 现有文件的 `apply_patch` 更新采用原位写入，保留所有权、硬链接和属性；
  不能把这个行为等同于跨文件原子事务或断电时的完整事务回滚。
- 终端输入阻塞不会再自动终止命令；输入可能保持挂起，调用方应根据
  `stdin_pending` 观察状态并决定下一步。
- 设备进程重启仍会失去内存中的终端会话。Agent 以 OS 用户身份执行，
  macOS 的 SIP/TCC 与 Linux 的原生权限仍有效。
- 审计日志不包含凭证正文、命令参数或文件内容；不是不可篡改的合规审计。
- 构建 amd64/arm64 发行版不等于在所有硬件实机运行；Docker Hub 容器
  与长期高并发压力测试不在本次回归覆盖范围内。

具体 Linux/macOS CI、发布归档及生产节点升级状态以本版本 GitHub Actions
和独立运行环境验收记录为准，不能仅由本地测试推断全部通过。
