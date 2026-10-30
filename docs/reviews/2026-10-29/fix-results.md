# SCOR-010～026 修复交接

2026-10-30 更新。本轮基于 `069b4f63dfed211bcf97ae8fbc2929d109675dd1` 修复，改动保留在工作区。官方对照提交仍为 `687f282e8625c6b8d11ff58211a90524be2e4317`。原始缺陷和修复前证据保留在 [主审查](../../REVIEW-2026-10-29.zh-CN.md)；[剩余能力](remaining-capabilities.md) 不因这批问题修复而自动完成。

联合验证：passed。17 项针对性回归及最终全量 race/vet 通过，二进制构建和实际 CLI 冒烟通过；下列 `implemented` 指本轮确认问题的修复结果，不代表 42 个 GAP 已完成。未访问真实账户或付费模型，没有外部发送测试数据。

## SCOR-010 — 会话所属的附件草稿

- severity: P1；decision: accepted；outcome: implemented。
- change: `cli/drafts.go` 以 Session ID 保存附件与 admission 元数据。turn 固定所属会话并捕获提交前缀；只有该次输入成功接纳或持久入队才 retire，失败和后加草稿不被清理。
- solution_fit: 适合现有 input batch/queue；成本 medium；solution_reliability: high；patch_scope: medium。
- regression_protection: `TestSCOR010DraftAttachmentsRemainWithTheirSession`、`TestSCOR010FailedSwitchAndOlderAdmissionPreserveDrafts`。原审查隔离测试已复现跨会话误发；本轮验证切换、新建、退回、失败切换及旧 admission 不误清新草稿。
- remaining_risk: 附件列表、单独移除和通用二进制上传仍由 GAP-010/011 跟踪。

## SCOR-011 — 不完整或无缩减的摘要不得替换上下文

- severity: P1；decision: accepted；outcome: implemented。
- change: `agent/compaction_request.go` 在持久替换前检查截断、空白、禁止的图片输出及带完整 checkpoint framing 的缩减量；取消或校验失败均保留原可见历史。
- solution_fit: 校验位于 CAS 之前，保持原始事件不变；成本 medium；solution_reliability: high；patch_scope: medium。
- regression_protection: `TestReviewCompactionRejectsInvalidCheckpoint` 和压缩边界回归。原隔离测试已复现 truncated/blank/larger 被接纳；旧成功 fixture 的历史扩大至能容纳官方 framing，未放松生产校验。
- remaining_risk: 模型摘要语义是否完整仍需真实任务差分；本修复保障可检测的结构、终止和缩减条件。

## SCOR-012 — 保留历史的输入分组与来源

- severity: P1；decision: accepted；outcome: implemented。
- change: 新压缩通过原子 `context/replace` 只替选中前缀；tail 保持原 origin/input group。旧 `context/compacted` 回放修正 retained input start 分组，同时保留旧 origin，避免破坏既存 CAS expected。
- solution_fit: 沿用现有 surface/CAS，不新增全日志迁移；成本 medium；solution_reliability: high；patch_scope: medium。
- regression_protection: `TestReviewPrunerAfterCompactionKeepsTailGroupsIndependent`，新增及 legacy surface 分组、来源和 input batch 边界回归。原隔离测试已复现压缩后工具结果无法独立剪枝。
- remaining_risk: 官方日志格式互操作另见 GAP-035。

## SCOR-013 — 可见上下文与目录驱动的指令恢复

- severity: P1；decision: accepted；outcome: implemented。
- change: `cli/instructions.go` 从当前可见 messages 查找基线，缓存同时记录 cwd/identity/digest；目录改变或基线被压缩后重新发布。`AfterCompaction` 在提交后、实际请求刷新前恢复基线，当前请求即可使用。
- solution_fit: 继续使用 durable 用户基线和现有加载器；成本 medium；solution_reliability: high；patch_scope: medium。
- regression_protection: `TestInstructionBaselineReturnsAfterVisibleCompactionAndResume`、`TestInstructionBaselineTracksWorkingDirectory`、`TestInstructionBaselineIsRestoredInSameRequestAfterAutomaticCompaction`；原隔离测试已复现仅首次检查导致缺失。
- remaining_risk: 文件操作后的嵌套指令发现与 changed/removed reconciliation 仍见 GAP-001。

## SCOR-014 — 摘要请求保留真实多模态块

- severity: P1；decision: accepted；outcome: implemented。
- change: 摘要请求重放原 system/tools 和选中消息前缀，末尾添加固定官方摘要指令；图片、文件、工具结果保持 block 形态。外源 opaque/signature 继续中和，摘要落为官方单 user checkpoint。
- solution_fit: 复用正常模型与附件投影路径；成本 medium；solution_reliability: high；patch_scope: medium。
- regression_protection: `TestReviewCompactionRetainsActualImageBlocks`、`TestCompactionNeverInterpolatesOpaqueReplayIntoPrompt` 及 envelope 回归；原隔离测试已复现只有 JSON transcript、没有实际 image block。
- remaining_risk: 辅助调用 usage 和完整 compaction 生命周期仍见 GAP-006。

## SCOR-015 — 可选摘要失败保留可用回合

- severity: P2；decision: accepted；outcome: implemented。
- change: 主动压力摘要失败发 warning，并刷新至最新已提交 surface 继续普通请求。overflow 恢复按持久变更判断进展，剪枝已提交而后续摘要失败时仍可重试；取消和必要的提交后恢复错误继续返回。
- solution_fit: 不撤销持久剪枝、不吞必要准备失败；成本 medium；solution_reliability: high；patch_scope: medium。
- regression_protection: `TestReviewPressureSummaryFailureDoesNotAbortUsableRequest`、overflow 剪枝进展/取消/hook 失败回归、`TestCompactionWarningIsVisibleAndTerminalSafe`。原隔离测试已复现摘要失败后普通请求调用次数为零。
- remaining_risk: 普通 provider 仍可能拒绝未进一步缩小的请求；恢复次数有界。

## SCOR-016 — HTTP 脱敏前识别上下文溢出

- severity: P2；decision: accepted；outcome: implemented。
- change: `core.IsContextWindowExceeded` 共用固定官方分类表达式；HTTP 适配器清除响应正文前产生稳定错误码，agent 使用同一分类。AUTH/QUOTA/RATE_LIMIT 优先级保留；不输出原始错误或凭据。
- solution_fit: 缩小到错误边界，无协议变更；成本 low；solution_reliability: high；patch_scope: small。
- regression_protection: `TestContextOverflowClassifiesBeforeRedaction` 覆盖 400/413、官方文本/类型码、普通错误及敏感文本不泄漏；原模型隔离复现已证明 Input too long for this model 被误分。
- remaining_risk: 新供应商错误变体须有证据后更新；不把任意 request-too-large 或 quota 当 context overflow。

## SCOR-017 — 同一步内执行溢出恢复重试

- severity: P2；decision: accepted；outcome: implemented。
- change: 模型 step 的恢复重试不推进 MaxSteps，也不重复 step-start；保持有界恢复，成功响应后重置连续溢出计数。
- solution_fit: 保留原执行预算和工具调度结构；成本 medium；solution_reliability: high；patch_scope: small。
- regression_protection: `TestReviewOverflowRetryDoesNotConsumeStepBudget` 与连续溢出边界测试；原隔离复现已证明 MaxSteps=1 时未重发即耗尽。
- remaining_risk: 不提供无限恢复；无持久进展或超过恢复次数仍返回错误。

## SCOR-018 — 附件和新用户终端使用当前目录

- severity: P2；decision: accepted；outcome: implemented。
- change: 附件相对路径和每次创建 Host terminal 查询 current working directory；终端面板使用 `ResolveCWD`，包括面板打开后后台发生的目录变化；原 workspace 沙箱边界保留。
- solution_fit: 复用 active/ensureWorkingDirectory；成本 low；solution_reliability: high；patch_scope: medium。
- regression_protection: `TestSCOR018AttachmentUsesCurrentWorkingDirectory`、`TestSCOR018HostTerminalCreateUsesCurrentWorkingDirectory`、`TestSCOR018TerminalPanelResolvesDirectoryAtEachCreate`。原附件隔离复现失败；新增真实 PTY/面板验证目录。测试以 os.SameFile 校验 macOS /var 与 /private/var 指向的实际目录身份。
- remaining_risk: Windows Host PTY 仍见 GAP-039。

## SCOR-019 — 附件打开不阻塞于 FIFO

- severity: P2；decision: accepted；outcome: implemented。
- change: Unix 使用 nonblocking open 后从 FD 检查普通文件、尺寸并有界读取；非 Unix 保留 preflight 及 FD 双检查，读取观察取消。
- solution_fit: 不靠文件扩展名推断类型；成本 low；solution_reliability: high（Unix）；patch_scope: small。
- regression_protection: `attachment_open_unix_test.go` 的 FIFO 回归和 `TestSCOR019AttachmentReadRejectsOversizeAndCancellation`；修复前已用隔离二进制实际复现 FIFO 两秒阻塞。
- remaining_risk: 非 Unix 的打开竞态保证和真实执行须在对应平台验收，不将 Unix O_NONBLOCK 的保证外推。

## SCOR-020 — 请求图片投影和定价共同接入生产路由

- severity: P2；decision: adjusted（已补齐原发现要求先确认的合同）；outcome: implemented。
- change: 官方 DeepSeek 路由共用实际模型目录、token-grid/4096 边长/2MiB 请求目标和同一 handle/access 文本；正常请求、摘要和工具多模态块均走 `ResolveProjected`。text-only/未编目模型替换为确定文字；旧 inline 保留原字节、按真实尺寸定价；file 使用真实发送的不可读路径 fallback 文本。
- solution_fit: 来源为固定提交 image-tokens/request-pricing/content，不套用第三方协议不存在的 DeepSeek 价格；成本 medium；solution_reliability: high（几何/文字/接线），medium（完整图像编码等价）；patch_scope: medium。
- regression_protection: `TestDeepSeekImagePricingPinnedGolden` 比较直接执行官方 TypeScript 生成的 512 组结果；`TestSCOR020*` 比较实际发送解码尺寸、价格、text-only、legacy、6 种 live/replay/pruned/native 内容形态及 canonical 历史不变。独立只读交叉审查核对 Files 上传/inline 回退不会二次换图。自审新增 `TestAttachmentHandlePreservesLiteralEscapesAndUnicode`，先复现再修复文件名中反斜杠与真实 Unicode 分隔符的混淆。
- verification_limit: 原发现是静态接线证据；本轮新增测试没有在旧提交宣称可直接编译运行。未进行真实 provider usage 对账。pi-ai 固定官方适配器本身没有精确 image override，保留其 heuristic，不能误称对所有模型精确定价。
- remaining_risk: route-priced 保留区、offload、可配置 image overrides、Sharp/Go codec 和色彩差异分别见 GAP-004/005/007/009/015。

## SCOR-021 — 实际模型请求使用有序预设提示词

- severity: P2；decision: accepted；outcome: implemented。
- change: `systemFor` 接入 `presetSystemPrompt`，保留技能目录，按 preset 独立边界组合 persona、plan 与工具段，移除旧方案的固定系统提示覆盖。
- solution_fit: 调用已存在构建器，成本 low；solution_reliability: high；patch_scope: small。
- regression_protection: `TestSystemForUsesOrderedPresetInActualRequestAndPreservesSkills`、`TestSystemForMinimalDoesNotReceiveStandardPolicy`，检查真实 Engine request 的顺序、技能只列目录及 minimal 边界；原证据为完整静态调用链。
- remaining_risk: 全部配置化提示词段与预设编辑仍见 GAP-032。

## SCOR-022 — 全局指令根与 DSH_HOME 一致

- severity: P2；decision: accepted；outcome: implemented。
- change: 指令加载器的 HarnessHome 明确使用当前 store.Root，与模型 Shell 受管环境一致，避免受 ambient HOME 的 .dsh 目录影响。
- solution_fit: 单一权威来源，成本 low；solution_reliability: high；patch_scope: small。
- regression_protection: `TestInstructionBaselineUsesAuthoritativeHarnessHome` 用两套冲突规则确认只采用实际数据根；修复前证据为配置及调用链检查。
- remaining_risk: 官方其他凭据与 .env 查找优先级仍见 GAP-017。

## SCOR-023 — JSON 聊天及斜杠输出保持 NDJSON

- severity: P2；decision: accepted；outcome: implemented。
- change: JSON chat 不写普通 banner/prompt；session/history/输出使用逐行 JSON 记录，print 不再写多行缩进 JSON。普通模式保持原显示入口。
- solution_fit: 集中输出边界，成本 low；solution_reliability: high；patch_scope: small。
- regression_protection: `TestJSONChatResumeAndSlashOutputIsNDJSON`、`TestJSONOutputAndNoticeEachUseOneRecord`；覆盖 resume/help/history/sessions/mode/export 与多行消息。原隔离复现已证明输出不能逐行解析。
- remaining_risk: 文档化 JSON event schema 的扩展版本策略不在这项修复内。

## SCOR-024 — Other 支持任意文本及多选附加答案

- severity: P2；decision: accepted；outcome: implemented。
- change: 明确 Other 选择后独立读自定义文本，所以纯数字和恰好等于 option label 的文本不再被强制解释；multi-select 可同时选择选项与 Other。计划仅按实际批准选项决定，不以 custom 内容批准。
- solution_fit: 保留 legacy 问答 schema，终端补足官方表达能力；成本 low；solution_reliability: high；patch_scope: small。
- regression_protection: `TestQuestionOtherPreservesNumericAndOptionLabelText`、`TestQuestionMultiSelectCombinesOptionsAndOther`、`TestQuestionSingleChoiceCannotCombineApprovalAndOther` 与 plan review 回归。原隔离测试已复现数字被拒。
- remaining_risk: opt-in timed questions 与 dismiss 计划后 concludeTurn 分别见 GAP-013/014。

## SCOR-025 — quoted 文件补全排除会话候选

- severity: P3；decision: accepted；outcome: implemented。
- change: 只有非 quoted 的 @ 查询进入 Session reference search，quoted 路径保留文件补全。
- solution_fit: 在候选来源边界判断，成本 low；solution_reliability: high；patch_scope: tiny。
- regression_protection: `drafts_references_test.go` 的 SCOR025 回归及现有 `TestCLICompletion*`；原隔离测试已复现 quoted 查询返回 dsh-session URI。
- remaining_risk: 无已知本项未修边界；完整文件导航见 GAP-020。

## SCOR-026 — 图片草稿按原始字节做批次 admission

- severity: P2；decision: accepted；outcome: implemented。
- change: 每个 Session 草稿独立保存 rawImageBytes，检查 20 张/200MiB 和单图限制，再标准化；模型与持久附件仍使用 normalized 引用，不把 raw 预算元数据写进模型请求。
- solution_fit: 与官方 raw batch admission 使用同一种计数口径，成本 low；solution_reliability: high；patch_scope: small。
- regression_protection: `TestSCOR026DraftImageBatchUsesRawAdmissionBytes`、`TestSCOR026DraftImageCountAndRawByteBoundary` 检查大量源字节/小引用、刚好边界、另一会话及下一批预算；修复前是静态数据来源证据。未虚构运行 200MiB 图像转码差分。
- remaining_risk: 部署级自定义图像限制另见 GAP-009。

## 联合验证与交付

- verification_result: **passed**；[最终运行记录](verification.txt) 保存全量输出和构建信息。
- `go test -race ./... -count=1 -timeout=60s`：全部包通过；CLI 50.235s、agent 13.077s、attachments 10.086s、MCP 25.932s；包括本机 HTTP、真实 PTY、macOS 沙箱及本轮永久回归。
- `go vet ./...`、`python3 scripts/verify-upstream.py`（六份固定输入）、`git diff --check`：均通过。
- 定向检查覆盖原隔离复现的压缩五项、指令恢复/目录/权威 home、真实请求系统提示词、NDJSON、Other/计划、会话附件/文件读取、补全和图片计量投影。512 组官方 TypeScript golden、6 种工具结果形态和文件名转义 fail-before/pass-after 均通过。
- 首轮全量的失败为测试兼容：预览测试依赖缩进 JSON；目录测试单边规范化 macOS alias。已改为解析元数据及 os.SameFile，未放松生产要求；修正后重新完成全量验证。
- 实际新二进制：隔离临时数据根运行 `--json chat` 的 help/history/quit，4 行逐行 JSON 通过；FIFO 附件拒绝约 0.041s，没有阻塞。未触发真实账号/模型请求。
- 已更新 `bin/dsgo` 及 `/Users/adib/.local/bin/dsgo`，PATH 指向后者，两份 SHA-256 均为 `e70dae3150428bdadc38d7cdf9d3d6193c5223950387ceb9a213d497160d0697`。版本仍为 `0.1.0-dev`，Go build info 标记 `069b4f6` + `vcs.modified=true`，不是宣称修复已提交。
- self-review: 保留 attachment/session ownership、原始审计日志、CAS/input group 与 route replay 隔离；摘要和定价共享实际投影；generic provider 无官方 image override 时保留其 fallback。未引入新依赖或外部插件。
- remaining IDs: 本轮 SCOR-010～026 无待修项；GAP-001～042 继续按各自状态跟踪。真实服务、完整编码/色彩等价及跨平台的限制没有被测试通过覆盖。工作区改动尚未提交。
