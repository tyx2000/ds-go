# dsgo

基于官方 DeepSeek Harness 移植的终端编码助手，启动命令为 `dsgo`。

定位是编码辅助工具：保留与官方一致的模型接口、上下文管理和编码相关工具，不追求官方 Web 的全部界面功能。定时提醒、文档预览、JS 工作流、内置终端面板、TypeScript 代码执行模式（PTC）和 `present` 交付物工具已移除，范围见 [功能对照表](docs/PARITY.md)。不包含插件市场、插件安装/开发或任意第三方 Harness 插件。

基线：[官方提交 `687f282e8625c6b8d11ff58211a90524be2e4317`](https://github.com/deepseek-ai/deepseek-harness/tree/687f282e8625c6b8d11ff58211a90524be2e4317)。主体为 Go 核心，不启动原 Web Host。

## 启动

需要 Go 1.26+ 编译，运行时不依赖 Node。macOS 使用系统 Seatbelt，Linux 受限执行需要 `bwrap`。

```sh
make build
./bin/dsgo
```

安装到 `~/.local/bin/dsgo`：

```sh
make install
dsgo
```

请确保 `~/.local/bin` 已在 `PATH` 中。也可以 `go install ./cmd/dsgo` 安装到自己的 `GOBIN`。

凭据可从官方账户或 API key 二选一配置：

```sh
dsgo auth login
dsgo
```

`auth login` 使用官方 PKCE 授权，浏览器用于登录确认，成功后回到终端。自动化可通过 `DEEPSEEK_API_KEY` / `DSGO_API_KEY` 提供 key；保存 key 时使用 `dsgo config set api_key --stdin`，避免把真实 key 写在进程参数中。

```sh
dsgo doctor
dsgo run "检查当前项目并解释入口"
dsgo run --json "读取 README 并给出摘要"
dsgo --resume latest
dsgo --cwd /path/to/workspace
dsgo models
```

`run` 为无人工交互模式，审批和问题不可用时会拒绝；需要人工决定的任务使用交互式 `dsgo`。`--json` 输出 NDJSON 事件。

## 交互命令

输入 `/help` 查看命令。支持多轮流输出和推理、`Ctrl+C` 取消当前轮、末尾 `\` 续行。

| 操作 | 命令 |
| --- | --- |
| 会话 | `/new`、`/sessions`、`/resume ID`、`/fork`、`/rename TITLE`、`/archive` |
| 历史与导出 | `/history`、`/export markdown`、`/export json PATH`、`/status` |
| 模型 | `/model NAME`、`/provider PROFILE`、`/thinking off\|low\|high\|max` |
| 权限与计划 | `/permission read-only\|workspace-write\|full-access`、`/plan on`、`/plan off` |

| 文件 | `/files PATH`、`/read PATH`、`/diff`、`/attach PATH` |
| 上下文 | `/compact`、`/continue`、`/skills`、`/skills NAME` |
| 任务 | `/goal TEXT`、`/goal status`、`/goal pause`、`/goal resume`、`/todo TEXT` |
| 后台任务与代理 | `/jobs`、`/jobs ID`、`/jobs kill ID`、`/agents` |

`/jobs` 列出模型在本会话启动的后台命令，`/jobs ID` 查看其已保留输出（不影响模型的读取位置），`/jobs kill ID` 停止它。后台命令属于当前进程，退出 dsgo 会结束。

## 数据与配置

默认数据目录 `~/.local/share/dsgo`，优先使用 `DSGO_HOME` 或 `--home`；遵循 `XDG_DATA_HOME`。凭据配置为 `0600`，数据目录为 `0700`。

```text
config.json
sessions/<id>/meta.json
sessions/<id>/events.jsonl
attachments/<sha256>
```

会话采用自己的追加式日志格式，保留失败/取消尝试，以已提交事件派生模型上下文。尚不支持直接读写官方原始历史目录。

```sh
dsgo config show
dsgo config set thinking high
dsgo sessions list
dsgo sessions search 项目名
dsgo sessions fork SESSION_ID
dsgo sessions export SESSION_ID markdown transcript.md
```

`config show` 遮蔽 key、账户 grant、MCP 环境和供应商配置中的凭据。运行时环境覆盖不会因为修改其他设置而保存到配置。

官方内置的其他供应商功能已按三种协议提供配置入口，支持情况见 [模型协议说明](docs/model-protocol.md)。不内置供应商市场、不安装额外工具。MCP 只连接用户明确配置的服务器，见 [MCP 范围](internal/mcp/README.md)；Skills 从已有文件发现，不执行安装流程。

## 验证与范围

```sh
make test
make check
```

协议测试使用确定性替身，不产生模型费用。macOS 上限制进程内再次启用 Seatbelt 的外层沙箱可能导致真实后端用例跳过，应单独在正常宿主执行该用例。真实账户授权与真实模型调用仍需用户自己的凭据验证。

仍需对齐的编码相关能力在 [对照表](docs/PARITY.md) 中跟踪。项目许可证及上游归属见 [LICENSE](LICENSE) 和 [第三方声明](THIRD_PARTY_NOTICES.md)。
