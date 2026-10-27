# 官方 Web 功能与 dsgo 终端移植对照

更新日期：2026-10-27（Asia/Shanghai）。当前状态：**实施中，尚未达到官方 Web 完整功能对等**。本表记录已实现的 Go 行为、可复核的测试入口和剩余工作，不是完整交付报告，也不以基本聊天可运行作为验收结论。

## 基线与范围

固定官方提交为 [`687f282e8625c6b8d11ff58211a90524be2e4317`](https://github.com/deepseek-ai/deepseek-harness/tree/687f282e8625c6b8d11ff58211a90524be2e4317)，不是跟随 `master` 的滚动承诺。本地保存了以下原始配置，许可证见 [upstream/LICENSE](upstream/LICENSE)：

- [base.cordis.patch.yml](upstream/base.cordis.patch.yml)：共享模型、会话、权限、工具及后台服务。
- [web-app.cordis.patch.yml](upstream/web-app.cordis.patch.yml)：Web 的 Host 服务、页面与默认预设注册。
- [standard.patch.yml](upstream/standard.patch.yml)、[ptc.patch.yml](upstream/ptc.patch.yml)、[minimal.patch.yml](upstream/minimal.patch.yml)、[cordis.patch.yml](upstream/cordis.patch.yml)：四个官方预设的明确工具和提示词组合。

官方 Web 配置会禁用共享层的若干 agent 工具，再由各会话的预设重新挂载；不能看到 `disabled: true` 就认定文件、子代理、计划、工作流等功能不属于 Web。当前默认预设是 `standard`，`ptc` 在类似组合上切换工具呈现，`minimal` 则具有独立 persona、受限文件工具和持久 Shell。`dsgo --mode ptc` 还不能代替这些预设的完整组合对齐。

这六份快照覆盖本表的静态配置清单，尚不代表已经穷尽仓库全部可选模块及部署覆盖层。`cordis` 保留与 `standard` 相同的主要固定使用模块，另加入 `tool-cordis`、预设创作 Skills 和插件管理/市场工具。后者的安装与扩展开发流程按用户范围排除，前者共有的对话、文件、计划、子代理、工作流、日程等能力仍纳入对应行。

用户明确排除插件市场、安装和开发、Creator 扩展创作、第三方 Harness 插件及任意 React/Cordis 扩展兼容。Go 使用固定内置模块承载原功能。**官方内置功能不会因为原仓库将其实现为插件而被一起排除**；例如 Shell、子代理和网页搜索的官方设置页仍属于待映射的使用功能。Skills 和已有 MCP 连接入口也保留在功能范围内，不为此安装额外服务器或扩展。

Web 配置中的 `desktop-product-telemetry`、`product-analytics`、`ui-sidebar-browser` 具有 Desktop 条件，默认 Web 不启用，不能把它们直接计入默认 Web 缺失数。六份快照没有 `LSP`/`tool-lsp` 条目；当前 dsgo 没有 LSP，但早期评估提及 LSP 不足以证明 `standard` 默认包含它，需以其他实际加载配置补证。

状态含义：

- **已实现／局部验证**：存在可执行路径及针对该行为的测试；不表示与官方全量差分通过。
- **部分**：已承载部分能力，表中列出的官方行为或终端操作仍有缺口。
- **缺失／待核对**：没有对应实现，或没有足够证据确认完成。
- **载体重建**：原网页/插件运行框架改用 Go 固定模块与终端交互；使用功能仍需逐项验收。

## 模型、上下文与会话

| 官方功能与配置依据 | 终端入口 / 当前实现 | 状态与证据 | 尚未对齐或验证 |
| --- | --- | --- | --- |
| `llm`、`llm-deepseek`、`llm-deepseek-account`、`deepseek-llm-api-extensions` | `dsgo`、`dsgo run`；Go Messages/SSE 适配器 | **已实现／局部验证**。`internal/model/client_test.go` 覆盖流分片、thinking/signature、工具参数、取消、重试、错误脱敏及 Files | 未用真实账号或 key 完成模型端到端验证；`dsh_session_log` / `dsh_plugin_packages` 等原生扩展与官方完整重试策略仍需核对 |
| `agent-default-model`、`ui-model-selection`、`ui-deepseek-model-label-1017` | `dsgo models [account]`、`/model`、`/thinking`、`/provider` | **部分**。内置目录、账户模型列表及最低客户端版本门控有 `catalog_test.go` | 完整官方模型能力目录、模型设置页和所有默认值尚未逐项对齐；支持字段不等于已由运行时按每个模型能力正确选用 |
| `authorization`、`credentials`、`deepseek-account`、账户设置页 | 环境 key、`config set api_key --stdin`、`auth login/logout/status/profile` | **已实现／局部验证**。PKCE/state、取消、凭据先保存再跳转、origin 限制有 `account_test.go` | 真实浏览器登录和账号服务未验；原 `.credentials.yaml`、项目/用户 `.env` 查找优先级及官方所有账户设置未完整迁移 |
| `llm-pi-ai`、模型设置页 | `provider_profiles`、`selected_provider`、`/provider`；OpenAI Completions/Responses、Anthropic Messages 三种配置协议 | **部分**。`provider_test.go` 验证协议、工具流、reasoning、凭据引用、重试和模型发现 | 未移植 pi-ai 全部供应商目录及原生认证。Bedrock SigV4、Vertex ADC、Azure 特例、Codex/OAuth 等不得冒称已支持；部分新客户端配置到 CLI 的接线仍需最终验证 |
| `agent`、`agent-loop`、`session-checkpoint-policy` | 多轮模型/工具循环、`--json` 事件流、Ctrl+C 取消 | **已实现／局部验证**。`engine_test.go`、`TestCLIConversationToolResumeAndJSON` | 模型/工具并发调度、所有官方提示词及事件 schema 未做全量黄金差分；当前顶层工具按序执行 |
| `session`、`session-persistence-jsonl`、`session-log-deepseek` | `--resume`、`sessions show`；追加式事件日志、settled message 与 attempt 区分 | **已实现／局部验证**。失败尝试不回放、取消补齐工具结果、崩溃边界修复及尾部损坏测试 | 使用 dsgo 自有事件格式；不支持直接读写官方历史、`session.vN.jsonl[.zstd]` 代际文件及其全部迁移策略 |
| `session-controller`、`session-log-download`、`ui-settings-session-log` | `sessions list/show/fork/export/rename/archive/unarchive/delete`，交互对应 slash 命令 | **部分**。存储测试覆盖分叉隔离、身份不可变、跨进程写入和格式导出 | 官方完整会话菜单、导出包内容、保留/清理设置、引用和冷会话控制未逐项差分；不是官方历史数据互操作声明 |
| `session-title`、`session-title-llm` | 首次提示的本地标题回退；`/rename` | **部分**。存在标题路径 | 未移植官方首轮 LLM 标题生成、字节上限和全部标题更新规则 |
| `session-query-sqlite`、`session-projection`、`session-projection-cache`、`storage*` | `sessions list/search/show`、`/sessions 查询`、`/history`、`/status`；Go JSON 元数据/事件投影 | **部分**。支持标题/工作目录子串检索；自有存储和模型历史投影已测 | 全部投影与缓存行为尚未齐全；默认 Web 全文搜索原本关闭，不能把未默认启用的全文索引当作默认已提供 |
| `token-meter`、`compaction-basic`、`command-compact`、`tool-result-pruner`、`image-offload` | `/compact`、自动摘要压缩、usage 事件 | **部分**。压缩事件可重建模型上下文 | 当前阈值含字节估计；官方精确计量、结果剪枝、旧图片卸载/重试、提示词与失败策略未完整对齐 |
| `agent-instructions`、`system-prompt`、预设 `persona`、`time-context` | 祖先 `AGENTS.md`、工作区上下文、当前时间工具和固定终端提示词 | **部分**。`internal/context` 已实现指令和技能发现 | 指令/技能发现边界、YAML frontmatter、时间段提示和所有预设 persona 尚未全量差分；下级 `AGENTS.md` 当前主要通过提示要求模型读取 |

## 文件、执行、权限和产物

| 官方功能与配置依据 | 终端入口 / 当前实现 | 状态与证据 | 尚未对齐或验证 |
| --- | --- | --- | --- |
| `tool-fs`、`tool-fs-search`、`fs-sandbox`、`fs-observation-policy` | 官方 `read`、`write`、`edit`（非 minimal 预设展示）；`list_directory`、`search_files`；`/files`、`/read`；旧 `read_file`/`write_file`/`edit_file` 只保留历史重放 | **已实现／局部验证**。原子写、精确编辑、歧义拒绝、symlink 与读/搜索边界测试；审批后在写入临界区重新校验观察版本（SCOR-001）、旧名称共用官方观察与版本保护（SCOR-002）、非普通文件以非阻塞方式打开并拒绝（SCOR-003），见 `file_regressions_test.go`、`file_open_unix_test.go` | 搜索语法及排序/忽略规则未完全对齐；进程内路径锁不能约束外部编辑器，最终依赖写入时的版本条件 |
| `working-directory`、`tool-working-directory`、`directory-picker`、`workspace-files` | `--cwd`、会话记录的工作目录、文件列表 | **部分** | 缺少完整工作目录模型工具与工作区切换流程；目录选择、文件变更推送、工作区页面操作仍未全量映射 |
| `sandbox*`、`approval`、`permission`、`subprocess` | `/permission`、`--permission`、一次审批；macOS Seatbelt、Linux bubblewrap | **已实现／局部验证**。拒绝/取消/审计失败不授予权限、真实 macOS 工作区写入边界测试 | Linux 真实系统集成尚待验证，Windows 受限执行未实现。这里是文件效果约束，不声称网络隔离；原策略名与全部平台后端不完全相同 |
| `tool-bash`、`tool-pwsh`、`shell-env`、`timeout-policy` | standard/ptc/cordis 的官方 `bash`（每次全新 `bash -c`，`description`/`workdir`/`timeoutMs`/`run_in_background`/`sandbox_permissions`）；旧 `run_shell` 只保留历史重放 | **部分／局部验证**。stdout/`[stderr]`/退出标记渲染、64,000 字节尾部保留与溢出文件、超时转后台 job、外部停止原因、沙箱拒绝推断与升级审批规则有 `standard_bash_test.go` | Windows PowerShell 路径缺失；`DSH_*` 托管环境（`shell-env`）与 `tool:bash` 系统提示段未接入；拒绝推断只按 stderr 特征；主动脱离进程组的子进程清理仍有限制 |
| `jobs`、`tool-jobs`、`job-controller`、`ui-jobs` | `job_output`、`job_list`、`job_kill`（bash、旧 Shell/PTY 与工作流 job 共用）；`/jobs` 通过旧视图观察 | **部分／局部验证**。会话所有权、消费 cursor、等待上限、`[status: …]` 渲染、已结束/取消请求结果有测试；bash 与后台工作流 job 结束时按 tool-jobs 原文通知所属会话（忙时下一步接收、空闲时唤醒），跳过被等待释放、模型自己 `job_kill` 和关闭时的结算，`dsgo run` 等通知处理完再退出，见 `TestJobNotices*`、`TestBackgroundWorkflowNoticesFollowJobRules` | 未实现 `maxConsecutiveWakes`、`completionDelivery: quiet` 和 `outputLimitBytes` 截断；旧 `run_shell` 后台 job 与 PTY 不发通知；完整实时任务面板未对齐；jobs 为进程内任务，不在重启后恢复 |
| `minimal` 的 `pty`、`persistent-bash` / `persistent-pwsh`；`ui-sidebar-terminal` | `terminal` 工具和 `/terminal` 的 start/list/read/write/resize/kill | **部分**。真实 PTY 的输入、尺寸、所有权及取消测试 | 尚无完整前台终端面板/键盘附着；未按 `minimal` 预设重建持久 Bash 命令契约、默认 300 秒策略与 PowerShell 对应实现 |
| `ptc-runtime`、`tools`、`ptc` 的 `tool-presentation` | `--mode ptc\|ptc-repl`、`run_code`；Node erasable TypeScript、工具绑定、每调用进程或会话内存 REPL | **已实现／局部验证**。TS/await、嵌套审批和统一 dispatcher、REPL 状态、取消清理、审计失败测试 | 不是与官方运行时逐语法/逐异常完全一致的声明；顶层 return 在两种运行时的约束、工具呈现/资源限额/所有生命周期仍需官方差分。Node 依赖是官方执行语言的承载，不是第三方 Harness 插件 |
| `spill-local`、`spill-policy`、`tool-result-pruner` | 顶层工具结果超限时保存完整原文；`/artifacts spill ID [OFFSET] [LIMIT]` 分页恢复；bash 每流保留 64,000 字节尾部并写溢出文件 | **部分／局部验证**。所有者绑定、SHA256 校验、UTF-8 边界和审计失败行为有 `artifacts_test.go`；溢出文件超过 64 MiB 时撤销“完整输出”引用（SCOR-007） | 当前阈值为 50,000 字节近似，未按官方 12,500 token 精确计量；旧结果剪枝、图片计入和单工具输出上限前的捕获未实现 |
| `present`、`ui-deliverables`、`workspace-changes` | `present`（1–8 个文件，记录 `deliverables/presented`）；逐轮工作区快照与 `/artifacts changes`、`/artifacts diff`；`/diff` | **部分／局部验证**。`present` 契约、逐轮快照（含新增、暂存、删除、二进制与超限文件且不运行 Git）有 `artifacts_test.go`、`cli/artifacts_test.go` | 交付卡片与文件预览联动、配额/保留/垃圾回收未实现；工作区之外的仓库文件和工具在工作区外的编辑不记录；不声称完全符合官方 Git ignore/rename 语义 |
| `attachment-local`、`file-upload`、`ui-attachment`、模型 Files | `/attach` 的文本/图片输入；`read_image`；内容寻址文件存储与出站引用解析；请求图片变体缓存；MCP 图片耐久投影 | **部分／局部验证**。持久引用、出站解析不改日志、校验和与 symlink 拒绝；GIF 按逻辑画布合成首帧（SCOR-008）；批量图片全部通过检查后才发布引用；请求变体身份、缓存损坏重建与并发一致，见 `attachments` 包测试和 `mcp_images_test.go` | 请求图片变体尚未接入按模型路由的选择；尺寸/token 定价、配额清理、gallery/lightbox、文档附件、完整 ICC 处理未齐全 |

## 计划、目标、子代理、日程和外部资料

| 官方功能与配置依据 | 终端入口 / 当前实现 | 状态与证据 | 尚未对齐或验证 |
| --- | --- | --- | --- |
| `plan-mode`、`ui-plan`、`user-questions`、预设 `tool-ask-user` | `/plan on\|off`、`exit_plan_mode`、`ask_user_question`；模式按会话持久保存 | **部分／局部验证**。迟到答案隔离、只读调查、拒绝权限升级、无人交互失败、计划模式恢复/分叉隔离有测试 | 全部问题组件/选择语义和官方计划事件尚未完整对应；当前终端控制及交互方式与 Web 不同 |
| `tool-todo` | `update_todo`、`/todo` | **部分**。Todo 可持久化为会话事件 | 官方 `todo_write` schema、完整结构编辑/并行进行状态规则及独立任务面板未全量对齐；`/todo TEXT` 的简单记录不是全部 Todo UI |
| `goal`、`goal-round-driver`、`command-goal`、`tool-goal`、`ui-goal` | `/goal` 创建/查看/暂停/继续，`update_goal` 更新已有目标，自动续轮 | **部分**。当前已具有持续执行路径，并拒绝模型自行创建/替换用户目标 | 精确 token 预算、子代理成本归属、阻塞轮次判定、预算终态/恢复和官方完整自主驱动策略未齐全；模型更新状态的约束仍需官方行为样例差分 |
| `subagent*`、`tool-subagent*`、`ui-subagent`、子代理模型设置 | 非 minimal 预设的官方 `subagent`、`subagent_fork`、`send_message`（`agent_id`）、`interrupt_agent`、`list_agents`（children/descendants）、按需 `list_subagent_models`；`/agents`；旧 `spawn_agent`/`wait_agent` 只保留历史重放 | **已实现／局部验证**。结算通知（结束语、失败诊断）写入父会话 inbox，父忙时下一步接收、空闲时唤醒；工作中的子代理在下一步接收 `send_message`，空闲子代理续跑；`interrupt_agent` 不波及后代，级联取消不再通知；`cwd`、默认深度 1、新会话一次性采样的精确路由策略及继承；`dsgo run` 等子代理结算后再退出。见 `subagent_contract_test.go`、`agents_test.go` | catalog 只在本进程内，重启后不列出也不冷恢复旧子代理；未实现子代理的委派范围声明、深度上下文提示、`agent_preset` 选择和外部后端；交互模式下被唤醒的回合不能用 Ctrl+C 单独取消，期间用户输入会因会话忙而失败 |
| `workflow-ptc`、`tool-workflow`、`ui-workflow-run` | `workflow` 工具（前台/后台）；`job_*` 与 `jobs` 观察后台运行；`/workflows` 重放持久展示记录 | **部分／局部验证**。官方参数、`agent`/`parallel`/`pipeline`/`phase`/`log`、FIFO 并发与上限、致命/空值规则、无损 JSON、取消清理、持久事件和重启后标记中断有 `workflow` 与 `cli/workflow_test.go`；控制帧超限时终止进程并报错（SCOR-004）；结束前等待成员后代清理完毕（SCOR-009）；后台运行按 tool-jobs 通知 | 结构化子任务仍用“要求输出 JSON + 事后校验”代替模型原生结构化输出及其重试；后台完成沿用 dsgo job 事件格式；运行/子代理的交互导航未实现。官方本身不恢复脚本状态，不列为缺口 |
| `schedule`、`tool-schedule`、`ui-schedule`、预设 `time-context` | 官方 `schedule_create/list/update/delete`；CLI create/update/get/history/delete；`daemon run` | **部分／局部验证**。标题、六个选择器、ISO 星期、整记录 CAS、物理删除、收据/保留/分页、归档与委派检查、跨重启和并发均有测试；CLI 验证收据先于模型结束落盘 | 同会话批量周期提醒、Host FIFO 和失败后的重试节奏尚未全量对齐；IANA 别名规范化等差异详见 [日程状态说明](../internal/schedule/README.md) |
| `web`、`web-search-deepseek`、`web-deepseek-api`、`web-fetch-http`、`tool-web` | `web_search`、`web_fetch`；官方 Messages 搜索和公共 HTTP 抓取 | **已实现／局部验证**。协议、结构化检索、多查询合并、凭据 origin、DNS pin、重定向和 HTML 限额有测试 | 官方 direct API 模块已有，但 CLI 尚未开放全部设置开关；HTML 到 Markdown 全量排版差分、完整 charset、真实账户/API 检索验收未完成。没有添加其他搜索供应商 |
| `mcp-resources`、官方已有 MCP 连接使用能力 | `mcp_servers` 配置；stdio / Streamable HTTP tools、resources/资源模板，统一工具审批 | **部分／局部验证**。握手、分页、并发 ID、取消、资源投影和 HTTP session 生命周期有测试；协议首选 2026-07-28 并接受 2025-11-25、2025-06-18、2025-03-26、2024-11-05；输入/输出 schema 编译与 `structuredContent` 校验、内容块校验、图片经持久通道投影，见 `schema_protocol_test.go`、`mcp_images_test.go` | 远端 OAuth、自动重连、HTTP GET/SSE 恢复未实现；需要 task 执行的工具会被拒绝；音频未投影。官方客户端不支持的 prompt templates/资源订阅不作为新增需求 |
| `skill`、`skill-filesystem`、`tool-skill`、`ui-skill` | `dsgo skills`、`/skills`、`load_skill` | **部分**。发现并读取本地 SKILL.md | 官方所有查找根、优先级、frontmatter、引用展开和选择器交互未做全量差分。没有技能安装/市场扩展能力 |
| `repeat-tool-reminder` | 引擎内置的重复调用提醒，按参数规范化后的调用计数在阈值处注入提醒 | **已实现／局部验证**。精确阈值、规范化参数、代理隔离、透明过滤及与 JavaScript JSON 规范一致有 `agent/repeat_test.go` | 提醒为建议性质、不阻止执行，与官方一致；配置项暂未暴露为终端设置 |

## 网页界面的终端承载与仍缺失交互

| 官方页面/服务 | 当前终端承载 | 状态与下一步 |
| --- | --- | --- |
| `ui-chat`、`ui-conversation`、`ui-tool`、`ui-trajectory`、会话统计/turn outline | 流式文本、工具启动/完成记录、JSON 事件、`/history`、`sessions show`；`/trajectory` 全屏轨迹浏览（turn 导航、记录检查、搜索、统计、纯文本与 `--json`） | **部分／局部验证**。投影、计数、失败/部分/恢复记录、PTC 并发配对、控制序列过滤、CJK 换行和真实 PTY 退出恢复有 `tui` 测试 | 主聊天仍是行输入；图形化时间轴缩放、图片 lightbox、JSON 树折叠、代码高亮/复制和旧历史分页加载未实现 |
| `ui-sidebar-files`、`workspace-files`、`ui-workspace`、`workspace-controller` | `/files`、`/read`、`--cwd` | **部分**。缺完整工作区/项目页面、目录选择与切换、文件树刷新/打开操作、会话引用和文件引用选择器 |
| `ui-sidebar-documentpreview`：Markdown、代码、纯文本 | `/read`；`/preview PATH [JSON]` 的分页、搜索和行定位 | **部分／局部验证**。文本分页、编码识别、搜索和取消有 `preview_test.go` | Markdown 仍主要显示源码；选择、与文件树联动尚未实现 |
| 同一预览：HTML、PDF、Office、表格、图片；`office-to-pdf` | `/preview`：Kitty/iTerm2 位图、HTML 截图与本地资源打包、PDF/Office 外部渲染接口、XLSX/CSV/TSV 解析 | **部分且仍在需求范围内**。HTML 静态/交互沙箱、资源打包、表格关系/公式/合并/冻结/样式读取、CSV 与资源上限有 `preview_test.go` | HTML 截图不能交互；PDF 缺与页面联动的文字选择；Office 未对齐官方 kit/字体诊断；XLS 不支持；表格未实现完整样式/冻结交互；本机缺 Poppler/LibreOffice，PDF/Office 路径未实际验收 |
| `ui-attachment`、`resources`、`ui-reference`、`session-reference`、`file-reference-local` | `/attach`、MCP 资源文字投影、文件路径 | **部分**。缺统一资源导航、拖放的键盘对应操作、会话/文件 `@` 选择与引用生命周期；MCP 图片/音频目前有明确缺口提示 |
| `ui-settings*`、`settings-controller`、`config-editor` | `dsgo config`、模型/权限命令 | **部分**。只暴露 Go 当前配置字段；General、Shell、Agent Loop、Subagent、Web Search、Session Log 的官方可用设置必须逐项补齐，不能因页面位于“内置插件”分组而排除 |
| `ui-agent-preset`、`agent-preset-registry` | `--mode native\|ptc\|ptc-repl` | **部分**。尚无与官方四个已保存预设相同的选择、默认值、persona/工具组合与每会话隔离；`minimal` 尤其不能用普通 native 模式代替；`cordis` 的扩展创作部分按用户范围单独排除 |
| `ui-theme`、`locale`、`shortcuts`、`ui-shortcuts`、`ui-input-trigger`、`ui-commands`、`ui-layout`、`ui-sidebar*` | slash 命令、行输入、多行续写、Ctrl+C | **部分**。缺命令/引用补全、可配置快捷键、完整本地化/主题、侧栏/面板等价操作；目前是终端命令交互，并非已完成所有 Web 操作的 TUI |
| `ui-secret-mask`、`message-feedback`、`command-feedback`、`ui-message-feedback`、`session-telemetry-otel` | 配置显示掩码、HTTP 错误脱敏、终端控制字符过滤 | **部分／缺失**。未实现发送文本中识别凭据的展示掩码、Like/Dislike、反馈对话框和用户明确提交后才进行的官方反馈导出；不会把这些基本输出保护计作反馈功能完成 |
| `open-in-app`、`ui-open-in-app` | 无对应菜单 | **缺失**。官方已解析应用的打开操作待映射；不自动以手工运行外部命令认定完成 |
| `web-startup`、`webserver`、`web-runtime`、`connection`、`api-remotes`、`typert*`、`remote-access`、各 controller、`modules`、`ui-renderer`、`ui-brand-official` | Go 本地进程直接调用固定模块 | **载体重建**。没有兼容原 HTTP/WebSocket/Mux、Cookie/Peer/远程访问协议的声明。SSH/远程工作区启动、重连等产品使用行为仍需另行明确和验证，不能仅凭本地 CLI 覆盖 |
| `plugin-manager`、`plugin-inventory`、`ui-plugin-manager`、`ui-settings-plugin-inventory`、`plugin-package-inventory-deepseek`、`hmr`、`client-hmr`、`cordis-*-runner`、`ui-cordis`、`harness-dev-context` | 固定模块、`doctor` 的基本运行环境信息 | 通用安装/开发/热加载及 Creator 扩展能力按用户范围排除；这些条目若还承担固定官方诊断/状态操作，须拆分核对，不能整行按“插件”二字判为完成或排除 |

## 验证边界与后续验收

现有测试验证 Go 自身的确定性协议和状态行为，包括假模型/HTTP stub、独立子进程 MCP stub、真实 Node、PTY，以及单独运行的 macOS 沙箱测试。测试文件是可重复的验收入口；它们不证明已经运行官方 Web 与 dsgo 的完整行为差分。`internal/model`、`internal/state`、`internal/tools`、`internal/mcp`、`internal/webtools`、`internal/agent`、`internal/cli`、`internal/schedule` 的局部结果也不能代替最终整套测试记录。

达到“完整终端移植”仍至少需要完成：

1. 补齐官方预设/配置清单，逐个核对默认启用、可选和平台条件；固定工具 schema、提示词、模型请求和事件黄金样例。
2. 完成表中缺失的功能与终端操作，尤其是工作流、完整设置/工作区/引用、产物、反馈和图形文档交互；不能通过从清单删除这些项达成完整性。
3. 对取消、迟到审批、审计失败、崩溃/重启、分叉、压缩、子代理清理、提醒投递和临界资源限额做官方与 Go 的同输入对照。
4. 完成真实账号/API、真实模型和网页检索端到端测试；在目标系统验证真实沙箱/PTY，明确尚不支持的平台。
5. 若要导入或共享官方历史，单列数据格式、迁移和写入所有权验收。当前自有存储只承诺 dsgo 的恢复与分叉。

不提供“全功能完成百分比”：固定功能尚有未盘点部分，按已写模块数或测试数计算百分比会掩盖交互和协议缺口。

## 静态配置 ID 清单

以下是六份本地快照中实际出现的 `id`，保留出现顺序。它是静态覆盖核对索引，不解析 `!!js` 条件，不等于运行时全部启用，也不等于这些行已经移植。组内重复/覆盖必须按 base → Web → preset 的组合语义解释。

<!-- ROSTER -->

### base.cordis.patch.yml

条目出现次数：96；SHA-256：`a2cc57868a4d016609801e28781262515483dde29b41c2742d9a3b27a6f52f77`。

```text
tool-plugin-manager plugin-manager timer hmr llm deepseek-llm-api-extensions session
session-log-deepseek typert typert-loader typert-gateway remote-access session-title session-title-llm
user-questions agent plugin-package-inventory-deepseek agent-default-model jobs llm-retry config-editor
settings authorization deepseek-account credentials llm-pi-ai session-persistence-jsonl attachment-local
session-query-sqlite working-directory tool-working-directory session-projection storage storage-json
storage-domain session-projection-cache otel session-telemetry-otel subprocess sandbox sandbox-policy
bash-sandbox pwsh-sandbox approval permission shell-env tool-bash tool-pwsh tool-jobs
fs-observation-policy tool-fs tool-fs-search agent-instructions skill skill-filesystem tool-skill
commands command-feedback goal goal-round-driver command-goal plan-mode token-meter compaction-basic
command-compact subagent subagent-spawn-in-process subagent-fork-in-process tool-subagent-control
tool-subagent-list-agents tool-subagent tool-subagent-fork ptc-runtime workflow-ptc tool-workflow
timeout-policy spill-local spill-policy session-checkpoint-policy tool-result-pruner image-offload
tool-todo tool-goal repeat-tool-reminder web web-search-deepseek web-deepseek-api web-fetch-http
tool-web mcp-resources tools system-prompt agent-loop fs-sandbox llm-deepseek llm-deepseek-account
```

### web-app.cordis.patch.yml

条目出现次数：116；SHA-256：`3e1f923309612ed8551141127409d662e78758e2d0d125218a3f70514417e472`。

```text
system-prompt session-query-sqlite tools desktop-product-telemetry product-analytics
subagent-model-selection-settings message-feedback session-log-download open-in-app ui-open-in-app
workspace session-reference file-reference-local session-stats session-turn-outline directory-picker
plugin-inventory session-controller job-controller terminal-controller workspace-files schedule
ui-settings-account account-controller settings-controller workspace-controller cordis-host-runner
cordis-inspect-providers web-startup webserver web-runtime harness-dev-context client-hmr modules
connection file-upload api-remotes cordis-client-runner ui-theme locale shortcuts ui-shortcuts ui-layout
ui-renderer ui-session resources ui-sidebar ui-sidebar-right office-to-pdf ui-sidebar-documentpreview
ui-sidebar-browser ui-sidebar-terminal ui-sidebar-files ui-settings ui-settings-general
ui-settings-models ui-plugin-manager ui-settings-plugin-inventory ui-conversation ui-approval ui-chat
ui-brand-official ui-attachment ui-tool ui-cordis ui-deliverables workspace-changes ui-workspace
ui-workflow-run ui-input-trigger ui-commands ui-skill ui-subagent ui-reference ui-secret-mask ui-jobs
ui-schedule ui-goal ui-message-feedback ui-model-selection ui-deepseek-model-label-1017 ui-permission
ui-agent-preset ui-settings-session-log ui-settings-plugins ui-settings-shell ui-settings-agent-loop
ui-settings-subagent ui-settings-web-search ui-plan ui-user-questions ui-trajectory tool-plugin-manager
tool-bash tool-pwsh tool-jobs tool-fs tool-fs-search skill-filesystem tool-skill command-goal tool-goal
plan-mode compaction-basic command-compact tool-result-pruner tool-subagent-control
tool-subagent-list-agents tool-subagent tool-subagent-fork workflow-ptc tool-workflow agent-instructions
tool-todo tool-web agent-preset-registry
```

### standard.patch.yml

条目出现次数：33；SHA-256：`0d398d2a6872a73a106cb3bd19303ecccf1637e1fcad4d0bf9d8a72d2c2c68f5`。

```text
preset-standard persona agent-instructions time-context tool-bash tool-pwsh tool-fs tool-fs-search
tool-jobs tool-schedule skill-filesystem tool-skill command-goal tool-goal planning plan-mode compaction
compaction-basic command-compact tool-result-pruner delegation tool-subagent-control
tool-subagent-list-agents tool-subagent tool-subagent-fork workflow-ptc tool-workflow tool-ask-user
tool-todo tool-web present tool-plugin-manager tool-plugin-market
```

### ptc.patch.yml

条目出现次数：34；SHA-256：`9e355c947bf61c3d2fe9bf3214b386f83c0d99d37579ce22576d4b82cb6e1a2c`。

```text
preset-ptc persona agent-instructions time-context tool-bash tool-pwsh tool-fs tool-fs-search tool-jobs
tool-schedule skill-filesystem tool-skill command-goal tool-goal planning plan-mode compaction
compaction-basic command-compact tool-result-pruner delegation tool-subagent-control
tool-subagent-list-agents tool-subagent tool-subagent-fork workflow-ptc tool-workflow tool-ask-user
tool-todo tool-web tool-presentation present tool-plugin-manager tool-plugin-market
```

### minimal.patch.yml

条目出现次数：12；SHA-256：`a2a9ba1c9c392a45466708f18bc0aefbf300577f9b544dc06049f73a8a5b3267`。

```text
preset-minimal persona tool-fs compaction compaction-basic command-compact persistent-shell pty
terminal-bash persistent-bash terminal-pwsh persistent-pwsh
```

### cordis.patch.yml

条目出现次数：34；SHA-256：`01b0fc94fd49136b332254a7ae5658e43ffe646f7248047dba65d499cfc74596`。

```text
preset-cordis persona agent-instructions time-context tool-bash tool-pwsh tool-fs tool-fs-search
tool-jobs tool-schedule command-goal tool-goal planning plan-mode compaction compaction-basic
command-compact tool-result-pruner delegation tool-subagent-control tool-subagent-list-agents
tool-subagent tool-subagent-fork workflow-ptc tool-workflow tool-ask-user tool-todo tool-web tool-cordis
skill-filesystem tool-skill present tool-plugin-manager tool-plugin-market
```
