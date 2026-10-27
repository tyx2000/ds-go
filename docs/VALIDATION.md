# 本地验证记录

日期：2026-10-26（Asia/Shanghai）。官方基线 `687f282e8625c6b8d11ff58211a90524be2e4317`。

环境：macOS arm64，Go 1.27.3，Node 24.22.1；项目最低 Go 1.26。PTY 使用 creack/pty v1.1.24，HTML 解析使用 x/net v0.61.0。

已运行的核心验证：

- `go test -race ./... -timeout=45s`：全部包通过。HTTP/PKCE 本地测试监听在受限环境被拒绝后，同命令在正常宿主环境通过；不涉及外部账户或付费模型。
- `go vet ./...`：通过。
- CLI 确定性整合：发起任务、Messages 请求、文件工具执行、结果回传、NDJSON、跨运行恢复。
- 会话：JSONL 尾片恢复、完整末行、损坏拒绝、并发 writer、分叉隔离、工具调用崩溃修复与 cancelled attempt。
- 取消与审批：迟到输入不会流入后一个问题；取消关闭尚未执行的工具结果；审计写入失败阻止执行；关闭后的引擎拒绝新工作。
- 子代理：fresh/fork、并发配额、队列、所有权、迟到 spawn、递归取消、结束通知与关闭，均经过竞态检查。
- 目标/计划：用户创建约束、自动续轮、完成与预算暂停；会话级计划持久化及分叉隔离。
- 图片：日志保存内容寻址引用，出站时才读取字节；校验和、symlink 与路径逃逸测试。
- TypeScript/Node：原生 TS 去类型、顶层 await、嵌套工具与审批、内存 REPL、多轮状态和取消清理。
- PTY：真实输入输出、窗口尺寸/resize、会话所有权、kill；macOS Seatbelt 写边界曾单独在正常宿主通过。
- 模型：Messages、Chat Completions、Responses、Anthropic、三类签名/加密推理隔离、重试/取消/半包、Files 与账户 PKCE stub。
- MCP：独立子进程与本地 HTTP/SSE fixture、握手、分页、取消、清理；Web 搜索与抓取使用离线 transport fixture。
- 官方输入：`python3 scripts/verify-upstream.py` 校验六份原始配置的 SHA-256 及 Git blob identity。
- 最终整合：新增官方提醒接口后，整套 `go test -race ./... -timeout=45s` 与 `go vet ./...` 再次通过。另验供应商配置从 CLI 正确选路和取用对应凭据。
- 构建与启动：`go build -trimpath -o bin/dsgo ./cmd/dsgo` 成功；`--version`、`doctor`、交互 `/status`、`/quit` 冒烟通过；命令已安装到 PATH 中的 `/Users/adib/.local/bin/dsgo`。

这是实现验证记录，不是官方 Web 与 Go 的全量行为差分报告。真实登录、真实模型/网页检索、Linux 沙箱及 Windows 后端尚未验证；图形文档预览等未完成功能不能计入通过。完整性边界以 [PARITY.md](PARITY.md) 为准。
