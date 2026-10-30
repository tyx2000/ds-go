# dsgo 尚未完整移植的官方 Web 能力

记录日期：2026-10-29；修复结果更新：2026-10-30（Asia/Shanghai）。本清单细化本次主审查的 12 类能力缺口；代码错误及其本轮修复结果仍由 [主审查](../../../docs/REVIEW-2026-10-29.zh-CN.md) 的 SCOR ID 跟踪。本文没有执行真实登录、付费模型请求、外部发送或测试进程。

官方基线固定为 [deepseek-harness `687f282e8625c6b8d11ff58211a90524be2e4317`](https://github.com/deepseek-ai/deepseek-harness/tree/687f282e8625c6b8d11ff58211a90524be2e4317)。下文每个官方源码路径均相对于该提交的仓库根目录；不是对滚动主分支的承诺。组合依据为 `packages/bundle/base/cordis.patch.yml`、`packages/bundle/web-app/cordis.patch.yml` 和仓库内四个官方 preset 的配置，项目保存了对应 [组合快照](../../upstream/)。Go 路径均相对于本项目根目录。

这里的“等价”是终端载体下可完成同一项官方使用操作并保持相关模型、权限、取消和持久化语义；不要求复刻 React、Cordis 插件框架或浏览器线协议。不能以一个现成 shell 命令、一个字段或一张截图，代替完整产品行为的验收。

## 状态、范围和优先级

| 标记 | 含义 |
| --- | --- |
| 未实现 | 尚无对应的完整执行路径或使用入口。 |
| 部分实现 | 已有明确入口，条目列出的官方行为仍缺失。 |
| 未真实验证 | 有实现及局部测试，但缺真实服务、平台或官方同输入差分；不等于未实现。 |
| 本轮待验证 | 本次已在修复或已落盘，等待主任务联合回归；不重复计作未实现。 |
| 默认 / 配置后 / 平台条件 | 分别指默认 Web 组合、用户启用官方已有可选配置、特定操作系统或运行环境。 |

优先级是后续移植顺序，不是 SCOR 缺陷严重级别：**P1** 为模型行为、数据完整性及完整交付验收的前置项；**P2** 为默认 Web 使用流程及显示能力；**P3** 为可选配置或特定平台的完整覆盖。平台或供应商被明确纳入交付环境时，其 P3 项也必须验收。

每项的“验收”均为尚需完成的标准或测试类型，不表示下列测试已存在或已经通过。GAP ID 供后续稳定引用；实现后应保留 ID 并更新状态。

## 本轮修复与已有能力：不要重复列缺失

- **SCOR-020，已修复／局部验证**：DeepSeek 图片请求几何与 token 定价、请求图片变体、计量和实际序列化共用图片投影，以及图片只读访问路径文本已接。当前入口包括 `internal/model/image_tokens.go`、`internal/attachments/projection.go`、`internal/attachments/request_images.go` 和 CLI 的计量接线；主任务已完成 512 组官方 golden、实际投影及六种工具内容形态的 canonical 保持测试。通用 `FileHandleText` 当前只是明确说明“无可读路径”的 fallback，**不能称为通用文件已可读**。旧图片 offload、Files 配额恢复和完整色彩处理仍分别由 GAP-007、008、009 跟踪；通用文件上传与可读投影仍由 GAP-010 跟踪。
- **SCOR-010/018/019/025/026，已修复并通过回归**：按 Session 隔离草稿附件、当前工作目录接线、Unix FIFO 防阻塞、quoted 文件补全和原始图片批次字节限制。它们是既有入口的修复，不再作为新能力缺失重复计数。单独删除草稿附件仍是 GAP-011。
- 子代理的 durable catalog、冷恢复、运行时 root/depth/ownership；goal 的轮数、CAS、authority 和 driver；activation-scoped `structured_output`；jobs 的 quiet/唤醒预算/输出限额；日程批次和收据；MCP supervisor/代次重连/配置重载/现代握手与 legacy fallback；原子 input batch、文件/会话引用和行编辑补全；Host 用户终端与发送文本显示掩码，都已有实际实现。剩余 UI 或真实服务验收不能把这些整个模块重新写成“无实现”。
- 官方使用启发式 token-meter 和 usage anchor；**精确 tokenizer 不是此提交额外规定的交付要求**。官方 goal 限额是轮数；**精确 token budget/子代理 token 成本归属也不是该 goal 合同**。官方 `structured_output` 是 activation 工具合同，不能擅自改成必须有 provider-native `response_format`。

## 1. 指令与 Skills 生命周期

### GAP-001 — 文件操作驱动的嵌套指令发现与 reconciliation

- **状态 / 条件 / 优先级**：部分实现；默认，具有文件系统 provider；P1。
- **官方证据**：`packages/context/agent-instructions/src/index.ts`、该包 `README.md` 的 Model Experience。初始基线之外，成功的文件操作使相关嵌套 AGENTS/CLAUDE、后续 changed/removed 及 resume reconciliation 生效；官方不连续监听所有外部编辑。
- **Go 已有 / 缺口**：`internal/context/instruction_files.go`、`internal/cli/instructions.go` 已有初始链、字节预算、版本与当前目录基线。SCOR-013 已修复基线在压缩后不可见的问题；仍缺成功 read/write/edit 对深层文件的发现及后续更新/移除回调。让模型自行读更深 AGENTS 的提示不能替代这一链路。
- **依赖**：统一文件工具成功结果与受影响路径；durable source/version 元数据；压缩后的有效 surface 判断。
- **验收 / 测试类型**：临时多层项目做单元及引擎集成测试：read 子目录文件后仅注入相关新增指令；成功 edit/remove 后下步更新；失败操作不误注入；cwd、重启、fork、压缩后同输入 source/version 与官方纯源码 golden 一致。

### GAP-002 — Skills 发现根、YAML 元数据、优先级及目录变化

- **状态 / 条件 / 优先级**：部分实现；具有 `skill-filesystem` 的 preset；P1。
- **官方证据**：`packages/skill/skill-filesystem/src/index.ts` 的 roots/provider/watch 配置与同包测试；`packages/skill/skill/src/index.ts` 的作用域 catalog。官方有项目、用户、自定义和 bundled roots，以及 catalog invalidation。
- **Go 已有 / 缺口**：`internal/context/context.go:Skills` 发现 `.agents/.claude/skills` 并简化解析 name/description；缺完整 YAML frontmatter、官方根与覆盖次序、preset 作用域合并、可配置根、watch 与 mutation 驱动刷新。不能仅因存在 Skills 列表认定相同 catalog。
- **依赖**：GAP-001 的 mutation 入口；明确安全 YAML 子集、路径与符号链接语义；CLI catalog 更新通知。
- **验收 / 测试类型**：将官方 roots/重复名称/多行 frontmatter/删除文件/watcher fixtures 转成表驱动与目录监听集成测试，比较最终 catalog、顺序、可调用性；监听取消和关闭后不得再发布变化。

### GAP-003 — 官方 `skill` 工具及 user/model invocation 合同

- **状态 / 条件 / 优先级**：部分实现；具有 `tool-skill`/技能使用入口的 preset；P1。
- **官方证据**：`packages/skill/tool-skill/src/index.ts`、`packages/skill/skill/src/index.ts` 的 `SkillInvocationPolicy`、`renderSkillContent`。模型与用户调用分别受 `modelInvocable/userInvocable` 控制；catalog 与显式调用有独立 source，输出共用 `<skill_content>` 形状。
- **Go 已有 / 缺口**：`load_skill`、`/skills` 与 `LoadSkill` 已可读正文；名称/schema、正文 envelope、相对资源基目录、显式用户触发、禁止模型调用的技能、catalog republish 与去重复加载语义未完整移植。
- **依赖**：GAP-002；输入 admission hook；仅固定官方 Skills 使用，不要求安装市场或执行扩展代码。
- **验收 / 测试类型**：schema/正文 golden，加用户显式调用和模型 tool-call 集成测试；同技能仅允许用户、仅允许模型、同 turn 重复引用、压缩与重启后 catalog 可见性分别与官方比较。

## 2. 上下文、压缩与图片请求恢复

### GAP-004 — 按精确模型路由选择压缩策略与摘要模型

- **状态 / 条件 / 优先级**：部分实现；默认压缩，模型覆盖项配置后生效；P1。
- **官方证据**：`packages/compaction/compaction-basic/src/config.ts` 的 `resolveConfig/resolveTargetPolicy/resolveCompactSpec`；同包 `types.ts`。可配置 `auto`、threshold/headroom、retainRatio 或 retainTokens、summary provider/model、maxTokens 和重试次数，并支持精确 provider/model 覆盖。
- **Go 已有 / 缺口**：`internal/agent/compaction_policy.go` 已接默认压力阈值、先 pruner、保留尾部、overflow 恢复；仍使用固定策略常量，`internal/cli/context_route.go` 对通用路由主要依赖 `context_tokens`。缺完整覆盖表、独立摘要模型/输出预算和官方配置校验；小窗口 fallback 也不等于官方 invalid-policy 行为。
- **依赖**：GAP-015 的实际模型能力目录；配置 schema 与迁移；summary 路由和取消传递。
- **验收 / 测试类型**：纯配置表驱动 golden + 假模型集成：精确路由命中/不命中、retainTokens 与 retainRatio 覆盖、非法组合拒绝、summary 用独立路由与预算，默认分支仍保留既有压力恢复测试。

### GAP-005 — route-priced 压缩保留区域及 `/compact` 全合同

- **状态 / 条件 / 优先级**：部分实现；默认及手动 `/compact`；P1。
- **官方证据**：`packages/compaction/compaction-basic/src/region.ts`、`packages/compaction/command-compact/src/index.ts`。保留区应使用实际 route pricing，手动压缩还区分 busy/cancelled/changed/summary/commit/persistence 等结果，并输出其来源摘要记录。
- **Go 已有 / 缺口**：`internal/agent/compaction_policy.go`、`core.ContextSurface` 与 CLI `/compact` 已有切分、不拆工具对/input group、CAS 和 durable bracket。SCOR-016/017 已通过本轮回归；仍需完整 route-priced 保留区接线，确认图片/文件 handle 和文本替代价格一致，并对齐命令参数、结果分类和反馈。
- **依赖**：SCOR-020 联合验证；GAP-004；统一使用计量与序列化的同一路由投影。
- **验收 / 测试类型**：同一历史在 image/text-only/另一供应商三条路由上比较保留序列；保留预算边界、输入批次、未闭工具对及无进展均做 golden；故障注入覆盖手动命令的全部结果分类。

### GAP-006 — 摘要辅助调用 usage 与完整 compaction 生命周期

- **状态 / 条件 / 优先级**：部分实现；自动或手动真正生成摘要时；P1。
- **官方证据**：`packages/compaction/compaction-basic/src/summarizer.ts`、`packages/compaction/compaction/src/types.ts`、`packages/session/session-checkpoint-policy/src/index.ts`。官方记录 compaction start/summary/end、选中 source seqs、摘要 route/maxTokens/usage 及原输出与安全投影，并要求模型/工具副作用前的 durability checkpoint。
- **Go 已有 / 缺口**：`internal/agent/compaction_request.go` 本轮已落 canonical `compactionInstruction`、单个 user checkpoint frame、摘要完成/缩小校验；opaque reasoning neutralization、严格 surface replacement 与 durable bracket 也已存在。剩余是辅助调用 usage/原输出事实，以及官方完整 start/summary/end/source 关联和 persistence-failure 可观察合同的核对；不再把摘要 prompt/checkpoint frame 写成未实现。这里的“结构化 checkpoint”指官方摘要/事件合同，**不要求添加 `response_format`**。
- **依赖**：GAP-004/005、GAP-035 的日志映射；不能为了兼容事件名破坏 Go 已有原子替换。
- **验收 / 测试类型**：摘要 prompt/安全输出与事件 golden；在 start、summary dispatch、replacement、end、fsync 各点故障注入，确认崩溃后可读、没有不持久的模型/工具副作用，usage 不冒充主对话调用。

### GAP-007 — 旧图片 offload 与请求/摘要错误恢复瀑布

- **状态 / 条件 / 优先级**：未实现；图像能力路由超出 request image count/bytes 时；P1。
- **官方证据**：`packages/compaction/compaction-image-offload/src/index.ts`、`image-offload.ts`、`projection.ts`；`packages/llm/llm-deepseek/src/images.ts`；`packages/llm/llm-pi-ai/src/context.ts`。`IMAGE_OFFLOAD_REQUIRED` 选择最旧 occurrence，记录 durable `image/offload`，以后所有路由投影为占位文字；同时覆盖 summary-error，不消耗普通 provider retry budget。
- **Go 已有 / 缺口**：图片持久化、变体、定价已有实现与局部验证（SCOR-020）；缺 occurrence 级 offload 决策、持久投影、错误瀑布和相应预算语义。删除源图或截断任意消息不是等价方案。
- **依赖**：SCOR-020、GAP-005；统一 occurrence 身份；保留历史、fork 和恢复仍能读原附件。
- **验收 / 测试类型**：假 provider 报 count/bytes overage，比较最旧 occurrences 和占位文本；agent 与 summary 两条链均测试，重启/fork/换路由不重发已 offload 图，连续无进展不无限重试。

### GAP-008 — DeepSeek Files API 持久复用、过期和配额恢复

- **状态 / 条件 / 优先级**：部分实现；DeepSeek 图片经 Files API 使用；P1。
- **官方证据**：`packages/llm/llm-deepseek/src/file-store.ts`、`upload-index.ts`、`files-api.ts`、`config.ts`。映射按 endpoint 与认证作用域隔离，支持过期/刷新、合并上传与取消、quotaCleanupBatch，并仅回收 Harness 自有文件后有界重试。
- **Go 已有 / 缺口**：`internal/model/files.go` 已有上传/读取/列表/删除和到期缓存；尚非官方持久 upload index、刷新与 quota recovery 合同。请求图片变体落盘不等于远端 file ID 的耐重启复用。
- **依赖**：SCOR-020；私有 route identity；Files 模拟服务。这里的配额是**服务端 Files 配额**，不是要求新增本地 GC。
- **验收 / 测试类型**：HTTP stub 覆盖同 scope 复用、换凭据/endpoint 隔离、restart、expiry margin、并发等待者独立取消；配额错误只能删明确自有文件并按官方次数重试，外部用户文件不得删除。

## 3. 附件、草稿和图片显示

### GAP-009 — 完整图片色彩/元数据处理与可配置图像限制

- **状态 / 条件 / 优先级**：部分实现；默认图片，部署覆盖项配置后；P1。
- **官方证据**：`packages/attachment/attachment-local/src/image.ts`、`normalization.ts`、`index.ts` 与 `tests/normalization.spec.ts`。除方向/尺寸外，还识别 ICC/colour space/depth/metadata，保持透明度，并允许配置 admission/normalization 及原生转换并发限制。
- **Go 已有 / 缺口**：`internal/attachments/images.go` 已有方向、动画首帧、像素/边长限制、标准化和透明图片 cwebp 路径；仍缺与 libvips 等价的完整 ICC/非 sRGB/样本深度处理和所有官方可配置限额。SCOR-026 只修默认原始批次字节计数。
- **依赖**：明确可支持的色彩管理库/原生依赖；配置 schema；SCOR-020 的变体一致性。
- **验收 / 测试类型**：带 ICC、CMYK、16-bit PNG、EXIF、透明/全不透明 alpha、极端比例、动画与坏图的 fixture 差分；比较视觉像素、保留 alpha、元数据清除、pass-through 条件和错误码，不凭压缩字节恰巧不同判定全部失败。

### GAP-010 — 通用二进制文件的 durable reference 与接收收据

- **状态 / 条件 / 优先级**：未实现；默认 Web 的非图片附件；P1。
- **官方证据**：`packages/client/file-upload/src/index.ts`、`http-route.ts`；`packages/attachment/attachment-local/src/file-store.ts`；`packages/api/session-controller/src/commands.ts:resolvePromptFileReceipts`；`ui-conversation/src/client/service.ts:sendSession`。所有非图片原样存储，receipts 精确属于接收 Session，失败 admission 不消费，成功队列/历史观察后退役。
- **Go 已有 / 缺口**：`/attach` 仍仅图片或 ≤1MiB 内联文本，带 NUL 文档被拒绝；缺通用 FileAttachmentRef、流式存储、文件名/媒体类型与 digest 校验、Session-owned 收据和模型/执行世界访问投影。不能把全文塞进 user text 当完整替代。
- **依赖**：GAP-011、035/036；复用原子 input batch 与成功接收后清草稿；本地终端可替换 HTTP 上传载体，但须保留流式和收据语义。
- **验收 / 测试类型**：PDF/ZIP/大文本/空文件/重名与特殊 Unicode 名称集成；跨 Session receipt 拒绝、失败重试/取消/队列删除、restart/fork/导出复用；大文件测试证明内存有界且保存字节一致。

### GAP-011 — 草稿附件查看、单独移除及失败恢复交互

- **状态 / 条件 / 优先级**：部分实现；默认输入 composer；P2。
- **官方证据**：`packages/client/ui-conversation/src/client/input/facade.ts:removeAttachment/pruneAttachments`、`input/hub.ts` 和 `packages/client/ui-attachment/src/client/ComposerAttachments.tsx`。附件 rail 可单独移除，adjudicating/submitting 阶段的操作受 gate，失败发送恢复与当前 draft 协调。
- **Go 已有 / 缺口**：`internal/cli/drafts.go` 本轮已隔离 Session 并按提交前缀 retire；仍无附件列表、预览/选择删除、当前 draft 状态展示以及完整 detached submission 恢复交互。不是继续缺 Session 隔离。
- **依赖**：GAP-010；明确 TUI 对附件动作的键盘/命令入口；复用已接 admission/queue receipt。
- **验收 / 测试类型**：纯状态机 + PTY：A/B 草稿隔离、删除指定项保留顺序、提交中删除被拒绝、发送失败后恢复、旧 admission 不误清后加附件；不允许隐式跨 Session 转移文件。

### GAP-012 — 消息/工具/轨迹图片 gallery 与 lightbox

- **状态 / 条件 / 优先级**：部分实现；图片附件存在时；P2。
- **官方证据**：`packages/client/ui-attachment/src/client/index.ts` 注册 conversation/user/tool/trajectory 多个图片位置；同包图片展示组件。官方展示并非只在工具中打印 attachment ID。
- **Go 已有 / 缺口**：图片可进模型上下文，`/preview` 可单独显示 Kitty/iTerm2 位图；缺从消息或轨迹进入同组图片、前后切换/缩放/原图访问和关联消息的终端操作。非图形终端应有清晰元数据及可用替代动作。
- **依赖**：GAP-011、020/021、034；终端图像协议探测与安全输出，不能自动打开外部 app。
- **验收 / 测试类型**：gallery 顺序与 source 导航单元测试；真实 PTY/支持图像协议终端的查看、返回、取消和窗口恢复；重启后的旧图仍可访问。

## 4. 问答与计划审阅

### GAP-013 — 可选 timed questions、超时继续和晚到答复

- **状态 / 条件 / 优先级**：未实现；`tool-ask-user` 配置 `mode: timed`，**默认是 legacy**；P3。
- **官方证据**：`packages/interaction/tool-ask-user/src/index.ts`、`timed.ts`；`packages/interaction/user-questions/src/types.ts`。timeout 为 -1 或合法正秒数，默认 timed timeout 120；超时返回 pending，晚到答复仍有 durable 投影与后续交付，不等于跳过。
- **Go 已有 / 缺口**：`internal/cli/questions.go` 已支持 legacy 多问题/选项；SCOR-024 已修复数字 Other 和多选附加文本。缺 timed schema、request identity、pending/answered 状态、晚到答复与终端非阻塞展示。
- **依赖**：输入 broker/受管 Inbox、durable projection；后台问题必须标明所属 Session。
- **验收 / 测试类型**：可控时钟状态机 + engine/PTY 集成：超时前/恰好超时/超时后答复、-1、取消、重启、同 call 去重及外部新输入竞争；和官方 legacy/timed fixture 分别比较。

### GAP-014 — dismiss 计划审阅后结束当前回合

- **状态 / 条件 / 优先级**：部分实现；plan mode 调用 `exit_plan_mode` 并由用户 dismiss；P1。
- **官方证据**：`packages/plan/plan-mode/src/index.ts` 的 `ASK_CANCELLED` 分支调用 `exec.concludeTurn()`，返回 approved=false，保留 plan active；turn cancel/provider teardown 的 abort 保持失败身份。
- **Go 已有 / 缺口**：`internal/cli/plan.go` 已有计划指导、审阅、批准下步生效和状态记录；缺独立的“关闭审阅、交还用户、正常 conclude 当前回合”操作。普通 Other 反馈与 cancel 不能混同 dismiss。
- **依赖**：GAP-013 之外的 legacy question-dismiss 状态也需支持；复用 engine 的既有 concludeTurn 机制，不新增 provider 能力。
- **验收 / 测试类型**：假模型精确调用次数测试与 PTY：dismiss 后无多余模型调用且 plan 不退出；approve/keep-planning/Other/turn cancel 分别有正确终态与记录。

## 5. 模型、凭据与账号

### GAP-015 — 账户/供应商能力目录完整驱动实际请求

- **状态 / 条件 / 优先级**：部分实现；默认 DeepSeek，其他 provider 配置后；P1。
- **官方证据**：`packages/llm/llm-deepseek-account/src/index.ts`、`config.ts`；`packages/llm/llm-deepseek/src/config.ts`、`request-pricing.ts`；`packages/llm/llm-pi-ai/src/catalog.ts`、`models.ts`。目录提供容量、输出保留、模态、推理档位、图片覆盖和系统/工具更新约束。
- **Go 已有 / 缺口**：`models [account]`、ModelInfo、最低客户端版本门控、路由选择与局部图片能力检查已存在；本轮 SCOR-020 已让官方账户目录快照驱动 context capacity、默认 maxTokens、image modality 和定价；仍缺完整推理档位、系统/工具更新约束、按模型 image overrides、目录刷新及其他供应商能力的统一决策。API-key builtin 与通用 provider 的 context_tokens 回退仍不能代替这些剩余合同。
- **依赖**：SCOR-020、GAP-004/008；缓存刷新与 credential 变化；模型不存在/降级时须可解释。
- **验收 / 测试类型**：mock catalog 动态改容量/模态/推理选项，检查下一实际请求和 meter 都使用同一快照；最低版本、未知模型、目录失败、用户覆盖和现有 history replay 的兼容性均覆盖。

### GAP-016 — 官方可配置 provider 的原生协议及认证链

- **状态 / 条件 / 优先级**：部分实现；`llm-pi-ai` 默认零路由，用户添加 profile 后才启用；P3，选用相关供应商即成为交付前置。
- **官方证据**：`packages/llm/llm-pi-ai/src/adapter.ts`、`auth.ts`、`config.ts`、`provider-data.generated.ts`、`login.ts`。原生目录包括 Bedrock、Google/Vertex、Azure 及带 OAuth 的路由；具体认证由对应 provider 与 credential/authorization seam 承担。
- **Go 已有 / 缺口**：`internal/model/provider.go` 支持 OpenAI Completions/Responses 与 Anthropic Messages 三种协议和配置 endpoint；缺 Bedrock 原生认证/请求、Vertex ADC、Google 原生协议、Azure 特例及官方 OAuth login/refresh 等已支持分支。兼容 endpoint 不能冒称所有 native provider 已对等。
- **依赖**：GAP-017；逐个 provider 明确协议/schema、原生 SDK 或等价实现；不能为了凑数引入官方未配置的新供应商功能。
- **验收 / 测试类型**：按固定 provider catalog 做能力矩阵；每个已宣称 provider 需 mock HTTP/request-signing/token refresh 和错误 golden，选用者再做 opt-in 真实端到端；未支持配置明确拒绝。

### GAP-017 — 官方凭据来源顺序、记录存储和热更新

- **状态 / 条件 / 优先级**：部分实现；默认 credentials-local，provider 引用按请求解析；P1。
- **官方证据**：`packages/credentials/credentials-local/src/index.ts` 明确 `.credentials.yaml > inherited env > invocation cwd/.env > DSH_HOME/.env`；跨进程写锁、保留无关 YAML、删除后不残留、reference/record 更新通知。
- **Go 已有 / 缺口**：`state.Config`、环境 key、`api_key_env`、`ProviderCredential` 与私有 replay identity 已有；缺官方 store/记录与优先级、项目/用户 dotenv、外部更改通知和相应设置操作。不能以设置一个 `APIKeyEnv` 字段代替这条完整链。
- **依赖**：安全 YAML/dotenv 解析、私有权限与原子写；GAP-016/018。继续遵守凭据不入历史/模型/日志，route identity 不成为猜密码校验器。
- **验收 / 测试类型**：隔离环境和临时目录表驱动覆盖每层覆盖/空值/删除/轮换；并发写保留其他项；HTTP stub 验证下一请求刷新、旧 opaque replay 不跨身份。

### GAP-018 — 账号设置操作与真实 PKCE 授权验收

- **状态 / 条件 / 优先级**：部分实现 / 未真实验证；官方 DeepSeek account 登录后；P1。
- **官方证据**：`packages/credentials/deepseek-account-platform/src/index.ts`、`protocol.ts`、`details.ts`、`logout.ts`；`packages/api/account-controller/src/index.ts`；`packages/client/ui-settings-account/src/client/index.ts`。Desktop-specific 授权分支有独立条件，不计作默认 Web 必做。
- **Go 已有 / 缺口**：`dsgo auth login/logout/status/profile`、PKCE/state、callback origin 限制、先保存 grant 后完成跳转均已实现；账户设置全部字段/状态变化/失败反馈未完整映射，真实平台登录/注销与过期状态尚无端到端记录。
- **依赖**：GAP-015/017；用户实际授权环境；不能把 profile HTTP stub 当真实登录通过。
- **验收 / 测试类型**：先做官方协议 fixture 差分，再在有授权的真实账户中记录登录、取消、拒绝、过期、退出和下一请求生效；设置入口缺项按 account-controller schema 核销。

### GAP-019 — 首轮 LLM 会话标题生成

- **状态 / 条件 / 优先级**：部分实现；默认 `session-title-first-prompt-llm` 且有可用模型；P2。
- **官方证据**：`packages/session/session-title-first-prompt-llm/src/index.ts` 与 `session-title-llm/src/index.ts`；base 组合启用。标题调用有独立输入字节、输出 token、timeout、路由准备与辅助 request 记录。
- **Go 已有 / 缺口**：首条提示截取的本地回退标题和 `/rename` 已有；未实现官方首轮标题模型调用、边界/回退与用户手动重命名的竞争策略。
- **依赖**：GAP-015；辅助调用日志与取消；不影响主回合 latency/失败终态。
- **验收 / 测试类型**：假模型比较 title prompt/envelope；多字节上限、空/失败/超时输出、人工 rename 竞争和重复恢复不得覆盖已确认标题；真实模型标题单列在 GAP-041。

## 6. 工作区、导航和远程 Host

### GAP-020 — 工作区/项目目录流程及实时文件树

- **状态 / 条件 / 优先级**：部分实现；默认 Web workspace/controller/files；P2。
- **官方证据**：`packages/workspace/workspace/src/index.ts`、`packages/api/workspace-controller/src/index.ts`、`packages/api/workspace-files/src/index.ts`；`packages/client/ui-workspace/src/client/index.ts`、`ui-sidebar-files/src/client/index.ts`；`host/directory-picker-auto/src/resolve.ts`。
- **Go 已有 / 缺口**：`--cwd`、`/cwd`、`/files`、文件工具和引用候选已存在；缺独立工作区/项目集合、目录选择/打开流程、实时树刷新、选中项到预览/消息的联动与目录变化的完整 UI。
- **依赖**：GAP-023～027 的预览、可取消文件变化订阅、工作区与 session cwd 的明确区别；本地/SSH 环境需用合适目录交互。
- **验收 / 测试类型**：临时文件树与 watcher 集成 + PTY 导航；外部创建/重命名/删除、agent writes、工作区切换、权限拒绝及失效目录均能更新；目录候选不读取文件正文。

### GAP-021 — 子代理/工作流/jobs 的交互导航面板

- **状态 / 条件 / 优先级**：部分实现；默认相关 controller/UI；P2。
- **官方证据**：`packages/client/ui-jobs/src/client/index.ts`、`packages/api/job-controller/src/index.ts`；`packages/client/ui-conversation/src/client/conversation/location-index.ts` 及会话导航；web-app 组合中的 subagent/workflow/jobs UI 注册。
- **Go 已有 / 缺口**：子代理与 workflow 的执行、ownership/恢复、jobs 查询/wait/quiet/唤醒上限已接；`/jobs`、`/sessions`、`/trajectory` 能读状态。缺实时面板、进入子会话并返回所属上下文、工作流/作业详情与工具结果联动。
- **依赖**：GAP-020/034；复用已有 durable catalog/controller 投影，不能重建另一套业务状态。
- **验收 / 测试类型**：纯投影 + PTY 场景：并行 jobs 状态刷新、child close/恢复、workflow 结束、后台输出、取消和返回；导航不改变子代理权限或自动启动冷会话。

### GAP-022 — 远程 Host 的产品操作与连接验收边界

- **状态 / 条件 / 优先级**：部分实现 / 未真实验证；Web Host 在 SSH/远程访问环境运行时；P3。
- **官方证据**：默认 `host/directory-picker-auto/src/resolve.ts` 在 SSH、非 loopback 或远程 authority 下选择 browse；`host/open-in-app/src/resolver.ts` 禁止远程误开本机 app；`api/remote-access/src/index.ts` 控制非 owner。另 `packages/ssh/ssh/README.md` 明确 fs/subprocess/sandbox SSH providers 需**自定义 profile**，不是默认 Web 自动启用。
- **Go 已有 / 缺口**：能在 SSH 登录的机器上运行本地 `dsgo`；这不等于已移植所有 remote Host 的工作区操作、连接状态/重连与授权资源流程。Go 直接调用固定模块无需照搬浏览器 Mux/Cookie 协议。
- **依赖**：GAP-020/030/038；先确定目标是远程 Host 的终端入口还是跨机 fs/process providers，避免扩大默认范围。
- **验收 / 测试类型**：真实 SSH/无 display 环境的断连、重连、取消、归属与无错误本机打开测试；只有明确启用自定义官方 SSH provider 时，才追加 helper digest/heartbeat/remote cleanup 的双机集成验收。

## 7. 文档预览

### GAP-023 — HTML 交互预览与资源生命周期

- **状态 / 条件 / 优先级**：部分实现；默认 document preview，HTML；P2。
- **官方证据**：`packages/client/ui-sidebar-documentpreview/src/client/html/HtmlBody.tsx`、`bootstrap.ts`、`pack.ts`。官方有可交互 HTML body、受控本地资源打包与组件生命周期。
- **Go 已有 / 缺口**：`internal/preview/html_assets.go`、`render.go` 可打包资源并调用 renderer 生成截图；不能进行页面点击、表单、滚动、状态变化及运行时错误交互。截图是静态承载，不能算完整交互预览。
- **依赖**：明确终端等价交互载体及可选浏览器辅助；资源作用域、网络策略和进程取消；GAP-020。
- **验收 / 测试类型**：HTML fixture 配合有界浏览器/交互终端测试，完成同一 click/input/scroll 流程；资源越界拒绝、远端资源策略、关闭后 worker/listener/临时资源释放可验证。

### GAP-024 — PDF 文本选择与页面联动

- **状态 / 条件 / 优先级**：部分实现；默认 PDF preview；P2。
- **官方证据**：`packages/client/ui-sidebar-documentpreview/src/client/pdf/text.ts` 用 PDF.js TextLayerBuilder，`pdf.tsx/document.ts` 关联 canvas/viewport/selection；关闭释放 observer 与 selection listener。
- **Go 已有 / 缺口**：`internal/preview/render.go` 和 `/preview` 支持页面渲染/翻页；缺页面位置与提取文字对应、选择/复制、搜索定位联动和持续查看状态。外部 PDF 工具可用性需实机验证。
- **依赖**：可靠 PDF raster/text extraction 后端及 GAP-020/034 的面板状态。
- **验收 / 测试类型**：含旋转/CJK/多栏/扫描页 PDF 的像素与 text mapping fixtures；终端按页搜索/选择/复制、缩放后定位、取消/关闭资源均有交互验证。

### GAP-025 — Office kit、转换缓存和缺字体诊断

- **状态 / 条件 / 优先级**：部分实现 / 未真实验证；DOC/DOCX/PPT/PPTX 与可用转换 backend；P2。
- **官方证据**：`packages/document/office-to-pdf/src/index.ts`、`queue.ts`、`identity.ts`；`ui-sidebar-documentpreview/src/client/office/index.ts`、`FontNotice.tsx`。官方基于 LibreOffice kit，存在转换并发/队列/reader/bytes 限制、generation/version 缓存、缺字体提示和授权文件读取。
- **Go 已有 / 缺口**：外部 renderer/LibreOffice 类调用接口已存在；未完整对齐官方 kit 错误、字体诊断、缓存代次、排队取消及资源限制；尚无本机 Office backend 的完整验收记录。
- **依赖**：可获得的转换与字体环境、GAP-024；源版本和转换失败不能被旧缓存遮盖。
- **验收 / 测试类型**：真实 Office fixtures 转 PDF，核对版式、字体缺失、失败诊断与 generation 换代；并发/取消/文件变动/转换器关闭做集成压力测试并验证临时文件清理。

### GAP-026 — XLS 与完整表格查看交互

- **状态 / 条件 / 优先级**：部分实现；默认 XLSX/XLS/CSV/TSV preview；P2。
- **官方证据**：`packages/client/ui-sidebar-documentpreview/src/client/excel/index.ts` 注册四种格式；`xls.ts`、`xlsx.ts`、`format.ts`、`excel.tsx` 处理解析、格式与展示。
- **Go 已有 / 缺口**：`internal/preview/sheet.go` 已解析 XLSX/CSV/TSV、公式/关系/合并/冻结等元数据；缺旧 XLS、完整值格式/样式显示、冻结与工作表导航的实际终端交互。读到 freeze 元数据不等于屏幕冻结可用。
- **依赖**：安全 XLS parser；预览表格状态、横向/纵向视口和大表资源限制。
- **验收 / 测试类型**：官方 parser corpus 与 Go 表驱动差分；日期/数字/公式缓存/合并/隐藏行列/多 sheet/XLS fixtures；PTY 验证冻结、定位、切 sheet 与大表取消，不要求新增官方没有的计算引擎。

### GAP-027 — Markdown/代码/文本的完整显示与资源联动

- **状态 / 条件 / 优先级**：部分实现；默认预览；P2。
- **官方证据**：`packages/client/ui-sidebar-documentpreview/src/client/markdown/MarkdownBody.tsx`、`path-images.ts`、`frontmatter.ts` 和同包文本/代码 renderer。
- **Go 已有 / 缺口**：分页、搜索、编码识别与行定位已存在；Markdown 主要仍显示源文本，缺正文结构/表格/相对图片和链接交互、frontmatter 展示与文件树联动，代码展示/复制体验尚未完整覆盖。
- **依赖**：GAP-020/034；终端 cell-width、路径解析和安全控制序列处理。
- **验收 / 测试类型**：Markdown/代码 golden + PTY，覆盖 CJK、复杂表格/代码块、相对资源、链接到具体文件/行、frontmatter、窄窗口与复制原文，保持原文件未被修改。

## 8. 反馈与应用打开

### GAP-028 — 消息 Like/Dislike 与 durable feedback 状态

- **状态 / 条件 / 优先级**：未实现；默认 message-feedback/UI；P2。
- **官方证据**：`packages/feedback/message-feedback/src/index.ts`、`types.ts`；`packages/client/ui-message-feedback/src/client/MessageFeedbackActions.tsx`。评分/备注/分类/版本有 durable put/delete、冲突与 cold-session 行为。
- **Go 已有 / 缺口**：没有对应消息评分命令或面板；现有 transcript/JSON export 和脱敏不等于反馈。需要消息身份定位、修改/撤回、失败反馈，不能仅打印“已点赞”。
- **依赖**：GAP-035；稳定 finalized message identity；不需要自动上传任何数据。
- **验收 / 测试类型**：状态机/存储测试覆盖 put/update/delete、版本冲突、cold/live 同步、无效目标与 UTF-8 note 限额；PTY 验证定位消息和反馈结果。

### GAP-029 — 用户明确提交的会话反馈与导出动作

- **状态 / 条件 / 优先级**：未实现；默认 `/feedback`/反馈表单，有明确提交动作时才执行相关收集；P2。
- **官方证据**：`packages/feedback/command-feedback/src/index.ts`；`ui-message-feedback/src/client/dialog.ts`、`FeedbackDialog.tsx`；`session-log-export/README.md` 明确仅打开/关闭反馈表单不导出或提交。
- **Go 已有 / 缺口**：没有反馈对话流程与官方提交/失败反馈、可审查日志包选择。不能用自动遥测替代用户触发反馈，也不能把普通过程日志当已授权发送。
- **依赖**：GAP-028/036；明确目标与用户确认，未提供真实目标前用受控 fake transport。
- **验收 / 测试类型**：对话状态机及 HTTP stub：打开/取消零发送，提交一次，失败可重试且不误报成功；导出内容/附件范围和实际提交目标可检查。真实提交只在用户明确授权的验收步骤执行。

### GAP-030 — `open-in-app` 目录、可用性判断和显式启动

- **状态 / 条件 / 优先级**：未实现；默认 Web、本地可启动的已安装应用；SSH/远程环境应隐藏或拒绝；P2。
- **官方证据**：`packages/host/open-in-app/src/index.ts`、`resolver.ts`；`packages/client/ui-open-in-app/src/client/index.ts`。有平台解析、已安装事实、probe/launch timeout 和远程 Host 限制。
- **Go 已有 / 缺口**：授权登录会打开 browser，但无对应工作区/资源的 app 选择及启动入口；让用户手写 `open`/`xdg-open` 不算产品流程完成。
- **依赖**：GAP-020/022；平台解析与目标校验；只在用户选择后启动。
- **验收 / 测试类型**：平台解析 fixture + fake launcher；本地存在/缺失/失败/超时、路径参数正确引用、SSH 不探测/误启动；实际系统 app 只做显式 opt-in smoke。

## 9. 设置、预设与终端交互映射

### GAP-031 — 官方设置字段、accepted-value、reset 与运行时生效

- **状态 / 条件 / 优先级**：部分实现；默认设置页面，仅显示实际 served namespace；P1。
- **官方证据**：`packages/api/settings-controller/src/index.ts`；`client/ui-settings-{general,shell,agent-loop,subagent,web-search,session-log}/src/client/`。例如 Shell 仅编辑 timeoutMs/maxOutputBytes，Agent Loop 编辑 maxParallelToolCalls；页面不是“把整个插件所有字段都暴露”的通用承诺。
- **Go 已有 / 缺口**：`config` 与多种 slash、jobs/subagent/terminal/time/reference 字段已有；缺逐页字段映射、运行时生效时点、保存失败维持 accepted value、独立 reset。顶层工具顺序执行也尚不能提供官方 parallel-safe 并发上限的行为。不要把已接 jobs/subagent 设置全部重列未实现。
- **依赖**：各能力自己的剩余 GAP；配置 schema、默认值与 live/next-turn 生效策略表；GAP-017。
- **验收 / 测试类型**：从官方 controller 枚举**实际可见字段**建立数据表，逐字段 save/reset/invalid/failure/restart/生效点集成；parallel-safe 调用的并发上限、非安全工具串行与取消需专测。

GAP-031 的初始字段核销表如下；服务 schema 还有字段并不表示这些页面都提供编辑项：

| 官方设置面 | 固定源码与明确可用操作 | Go 当前及剩余核销 |
| --- | --- | --- |
| General / Appearance / Language / Shortcuts | `ui-settings-general/src/client/index.ts` 注册版本、开发工具模式、设置文档动作；`locale/src/client/index.ts` 提供语言偏好；`ui-theme/src/theme-settings.ts`/`src/client/index.ts` 提供 system/light/dark 和 text/code/terminal 字体角色；`ui-shortcuts` 提供编辑/冲突/重置。路径前缀均为 `packages/client/`。 | 版本/doctor、language 配置及固定键已有；全界面语言、可保存外观/键位、设置文档安全编辑和失败状态未齐。终端字体像素不能原样照搬，应明确哪些操作可由终端等价控制、哪些属于宿主终端设置，不能伪装为已支持的无效字段。Desktop 更新项依原条件排除。 |
| Shell | `packages/client/ui-settings-shell/src/client/shell-card-controller.ts`：served `bash-sandbox` 或 `pwsh-sandbox` 的 `timeoutMs`、`maxOutputBytes`。 | 命令级 timeout/输出处理已有；官方设置层默认/覆盖、独立 reset、保存失败维持 accepted value、下一命令实际取值仍需完整接线。不是要求对全部 Shell executor schema 无差别加 UI。 |
| Agent Loop | `packages/client/ui-settings-agent-loop/src/client/agent-loop-card-controller.ts`：`maxParallelToolCalls`。 | 当前顶层 tool dispatch 顺序执行；需要并行安全调用调度、上限设置、非安全调用屏障和取消语义，而非仅新增一个数字字段。 |
| Subagent | `subagent-limits-card-controller.ts`：`maxDepth`、`maxActiveSubagents`；`subagent-model-selection-card-controller.ts`：`enabled`、精确 `allowedModels[{provider,model}]`。两者均在 `packages/client/ui-settings-subagent/src/client/`。 | 深度/容量/model allowlist 与 sampled policy 已有实现；剩余是字段名/默认/边界逐项差分、配置入口的 accepted/reset/冲突行为及新旧 Session 生效点，不再把这些后端限制整体称作未实现。 |
| Web Search | `packages/client/ui-settings-web-search/src/client/web-search-card-controller.ts`：选择 Messages+local HTTP 或 Search/Fetch API；`web.searchProvider/fetchProvider`，Messages `baseURL/maxUses/apiKeyEnv`，API `baseURL/count/apiKeyEnv`，经 credentials 管理密钥状态与写入。 | `internal/webtools.Options` 已有相关搜索/抓取分支和参数；缺统一的用户配置/credential staged 保存、恢复默认、所选 API 的字段显示及整个保存事务。不能把支持 direct API 的库路径当完整设置页。 |
| Session Log | `packages/client/ui-settings-session-log/src/client/index.ts`：`session-log-deepseek.enabled`，附保存失败 toast；并非通用历史自动删除页。 | 见 GAP-037：缺实际增量贡献开关及 accepted/failure 状态；已有本地日志和日程历史保留不是该字段的实现。 |

### GAP-032 — 已保存预设与提示词段的查看/编辑使用流程

- **状态 / 条件 / 优先级**：部分实现；默认四种官方 presets，允许的用户配置；P2。
- **官方证据**：`packages/preset/agent-preset-registry/src/index.ts`、`agent-preset/src/index.ts`；`packages/client/ui-agent-preset/src/client/index.ts`；官方 standard/ptc/minimal/cordis 组合。
- **Go 已有 / 缺口**：`--preset`、`/preset`、工具/persona/执行模式和每 Session 选择已有；官方系统提示词接线由本轮 SCOR-021 跟踪（SCOR-013 是指令基线恢复，不能混号）。仍缺可审查的已保存预设/提示词段配置入口和全部默认值逐项对照。不能说四 preset 完全不存在，也不需要实现 Creator/插件创作。
- **依赖**：GAP-031；固定可编辑表面与版本；保留 minimal 的独立能力边界。
- **验收 / 测试类型**：四 preset 最终 request system/tools/config 的 golden；编辑保存、选择/新 Session/child 继承、无效配置回退和 restart；排除市场安装、任意 Cordis/React 扩展。

### GAP-033 — 完整本地化、快捷键配置与布局/主题动作

- **状态 / 条件 / 优先级**：部分实现；默认 locale/shortcuts/theme/layout；P2。
- **官方证据**：`packages/client/shortcuts/src/client/index.ts`、`ui-shortcuts/src/client/index.ts`、`locale/src/client/index.ts`；Web 的 shortcuts 语言/冲突/保存 snapshots。Desktop 专属项按原条件排除。
- **Go 已有 / 缺口**：Language 字段、固定快捷键、CJK 行编辑、pager/terminal 面板已有；缺界面全量中英文、快捷键 rebinding/conflict/reset、主题及相关 panel 开合/布局动作的终端等价映射。
- **依赖**：GAP-020/021/034；统一 action catalog；不可让自定义快捷键破坏 raw TTY 恢复和输入隐私。
- **验收 / 测试类型**：locale key 完整性和快捷键冲突表驱动；PTY 覆盖新绑定/恢复默认/保存失败、中英窄屏、终端 detach 与取消；界面显示改变不修改导出和模型原文。

### GAP-034 — 主聊天、长历史与消息动作的完整终端承载

- **状态 / 条件 / 优先级**：部分实现；默认 conversation/tool/trajectory；P2。
- **官方证据**：`packages/client/ui-conversation/src/client/`、`packages/client/ui-trajectory/src/client/` 与其消息/工具 renderer；`ui-attachment` 图片注册。官方可导航旧历史、展开结构化内容并对消息执行动作。
- **Go 已有 / 缺口**：流式聊天、NDJSON、history、全屏 trajectory/搜索/统计、tool 记录及 sent-user secret mask 已有；缺主聊天历史分页/定位、JSON 树折叠、完整代码显示/复制、消息动作与面板协调。不能把已修 NDJSON 或显示掩码继续列缺失。
- **依赖**：GAP-012/020/021/027/028；长期历史分页接口与可取消投影。
- **验收 / 测试类型**：大历史与多种 tool/media 内容的投影测试 + 真实 PTY；分页不丢事件/不重复，展开/复制与原文一致，CJK/resize/后台输出不破坏当前输入，所有退出路径恢复终端。

## 10. 会话数据、导出与日志策略

### GAP-035 — 官方代际会话日志的读取/迁移/互操作

- **状态 / 条件 / 优先级**：部分实现；默认 session-persistence-jsonl；P1。
- **官方证据**：`packages/session/session-persistence-jsonl/src/index.ts`、该包 `README.md`。最高 canonical generation 选择、`session.vN.jsonl[.zstd]`、历史 codec 迁移、读不发布/写发布 successor、torn frame 恢复与 committed corruption 拒绝有明确合同。
- **Go 已有 / 缺口**：`internal/state/store.go` 已有自有 JSONL、锁、fsync、尾损坏恢复、fork/input batch/投影。它不直接兼容官方日志格式、完整 generations/codec/migration 与全部投影，因此不能把 Go session 可恢复等同官方历史可导入。
- **依赖**：官方 event/identity 到 Go 的明确映射；GAP-006/010/028；选择兼容读写或有损性明确的显式 importer，不能静默改原日志。
- **验收 / 测试类型**：官方各代 fixture corpus 只读解码与迁移 golden；校验 source byte-identical、最高代选择、结构拒绝与 torn tail/压缩 frame 区分、失败发布和 source drift；跨官方/Go 导入恢复的端到端记录。

### GAP-036 — 包含子会话与所有附件的完整导出包

- **状态 / 条件 / 优先级**：部分实现；默认 Web session-log-export；P1。
- **官方证据**：`packages/session-query/session-log-export/src/archive.ts`、`src/index.ts`、`README.md`。ZIP 包含 session tree、canonical generation 文件、图片 media 与 generic files；流式读取大文件，导出前 flush live root，重复操作共享进行中的下载。
- **Go 已有 / 缺口**：`sessions export`/`/export` 有 JSON/文本/Markdown 形式，但缺官方 tree+附件归档、完整内容选择、流式打包、live snapshot 一致性及取消/重复操作状态。终端可选择路径，无需复刻浏览器下载目的地 UI。
- **依赖**：GAP-010/035；共享附件不可因导出移动或删除；安全归档路径和私有权限。
- **验收 / 测试类型**：多层 child+图片+大二进制文件导出 ZIP 检查，逐 digest 验字节、路径、源 generation 与最新 committed prefix；并发导出、文件缺失/权限错误、取消和有界内存集成。

### GAP-037 — 官方增量 Session Log 贡献及启停设置

- **状态 / 条件 / 优先级**：未完整实现；官方 DeepSeek request extension，`session-log-deepseek.enabled` 默认 true、可关闭；P1。
- **官方证据**：`packages/session/session-log-deepseek/src/index.ts` 的 `dsh_session_log` contribution、maxBytes、accepted cursor；`packages/client/ui-settings-session-log/README.md` 明确页面开关编辑 enabled，OpenTelemetry 是另一项。
- **Go 已有 / 缺口**：Go 有本地完整日志与模型 request 记录，但未接官方按成功接受前缀推进的增量上传协议、大小上限与设置生效。手工 extras 字段不等于这套自动 cursor 协议。
- **依赖**：GAP-035 的事件外部表示与 GAP-015；须明确告知并提供官方相同启停操作，不能因写文档触发任何上传。
- **验收 / 测试类型**：HTTP stub 比较 first/resume/failed request cursor、maxBytes/过大单事件、关闭后零贡献和重新启用；真实发送只纳入已授权 GAP-041。

**保留/清理边界**：本轮未找到足够源码证明默认 Web 提供通用“按天自动删除所有 Session/本地附件”的产品合同，故不把它写成确认缺失。`attachment-local/README.md` 的 Future: retention and remote storage 明确本地 reference-aware GC 自身 deferred，仍保留全部 immutable 对象。GAP-008 的远端 Files 配额清理、已有日程 delivery history 保留、Host terminal idle 回收是三件不同且已有明确来源的能力。

## 11. 子进程环境与平台覆盖

### GAP-038 — 官方 HTTP proxy policy 的子进程 overlay

- **状态 / 条件 / 优先级**：部分实现；启动环境配置 HTTP_PROXY/HTTPS_PROXY/NO_PROXY 等时；P1。
- **官方证据**：`packages/util/http-proxy/src/index.ts`、`install.ts`、`policy.ts`。启动时解析统一 policy；`proxyEnvironmentForChild`、`clearedProxyEnv` 区分真实子进程和重放环境，各 HTTP consumer 使用相同决策。
- **Go 已有 / 缺口**：Go HTTP transport 可按自身规则用代理，模型 Shell 的受管 DSH_* overlay 已接；仍未移植官方完整代理优先级/matching/子进程 overlay 与 replay 清理。不能因为环境变量自然继承就宣称与官方 resolver 完全相同。
- **依赖**：共享不可变 launch-environment snapshot；所有 HTTP client 与 subprocess 路径，尤其 MCP/PTC/model bash/Host user terminal 的独立归属。
- **验收 / 测试类型**：官方 matcher corpus 到 Go golden；本地假代理 + 子进程 env fixture 验证大小写、NO_PROXY、localhost、认证 URL 和清理；日志不得包含 proxy secret。

### GAP-039 — Windows 原生执行/终端/受限后端

- **状态 / 条件 / 优先级**：未实现或部分实现；Windows 条件；P3，Windows 交付时 P1。
- **官方证据**：base 组合按平台选择 `pwsh-sandbox`；`packages/subprocess/subprocess-local/src/terminal.ts` 有 Windows terminal cleanup；`packages/client/ui-settings-shell/src/client/shell-card-controller.ts` 服务 pwsh namespace；对应 sandbox 平台实现。
- **Go 已有 / 缺口**：目前主要原生执行/TTY 验证在 macOS/Linux；`internal/tools/terminal_controller_other.go` 对 Windows 拒绝 Host PTY，缺官方 PowerShell/Windows PTY、进程树清理和相应受限执行使用流程。仅 `runtime.GOOS` 分支或编译成功不算支持。
- **依赖**：ConPTY/Windows process identity 与任务清理、PowerShell schema/env/编码合同、实际 Windows runner。
- **验收 / 测试类型**：Windows build+native integration+真实 terminal：PowerShell 命令/取消/子进程、PTY resize/detach/reconnect、路径/Unicode、受限写入与失败诊断；禁止在不支持环境下静默降级为 unrestricted。

### GAP-040 — Linux 真实 bubblewrap 与 Host terminal 完整验收

- **状态 / 条件 / 优先级**：未真实验证；Linux 且受限 backend 可安装；P1 的平台验收项。
- **官方证据**：base 的 POSIX shell/sandbox 组合与 `packages/sandbox/` 对应 Linux provider、`packages/subprocess/subprocess-local/src/terminal.ts`。这类能力受 kernel/user namespace/bwrap 环境影响，不能从 macOS 测试推定。
- **Go 已有 / 缺口**：`internal/tools/process_unix.go`、Linux terminal/controller 与受限后端已有路径；缺已记录的目标 Linux 真环境运行、网络/写根限制、进程清理及 PTC/child/workflow 联合验证。此项不是“没有 Linux 实现”。
- **依赖**：有 bwrap/user namespace 的真实 Linux 环境与明确安装依赖；GAP-038。
- **验收 / 测试类型**：Linux 原生集成：read-only/workspace-write/full-access 允许/拒绝矩阵，跨 symlink/临时目录/网络，后台进程/取消/父进程退出，模型 bash 与 Host terminal 生命周期分离；保存环境、版本和结果。

## 12. 真实服务与完整交付验证

### GAP-041 — 官方账户、真实 DeepSeek 模型与会话恢复差分

- **状态 / 条件 / 优先级**：未真实验证；用户有授权账号/key与可用额度；P1 发布门槛。
- **官方证据**：固定提交的 `deepseek-account-platform`、`llm-deepseek-account`、`llm-deepseek`、`session-title-first-prompt-llm` 与 base/web 组合。HTTP fixtures 只能证明已编码合同，不能证明真实服务仍接受完整请求。
- **Go 已有 / 缺口**：账号、catalog、SSE/tool/reasoning、重试、opaque replay 隔离、恢复都有实现与局部测试；缺官方 Web 与 dsgo 对同一小型任务的真实 request/response/终态对照，含取消/网络失败/恢复。不要因此重新列成“账号/流式聊天未实现”。
- **依赖**：GAP-015/017/018/019/037；SCOR 全部目标修复通过；额度与网络由真实环境提供。
- **验收 / 测试类型**：显式 opt-in live 测试计划与脱敏记录：登录、目录、文本/推理/tool、取消、恢复、切 endpoint/credential；比较可观察行为与关键字段，不要求随机生成文本逐字相同；记录失败、实际花费和未验证分支。

### GAP-042 — 真实多模态、Web Search、配置 provider 与全产品流程

- **状态 / 条件 / 优先级**：未真实验证；对应模型/服务/平台条件满足；P1 发布门槛。
- **官方证据**：固定提交的 `llm-deepseek/src/images.ts`、`file-store.ts`、`web/web-search-deepseek`、配置 provider 及四 preset 组合。网页搜索/Files 的 HTTP stub、纯源码 golden 和真实 Node/PTY 测试各自覆盖不同层。
- **Go 已有 / 缺口**：`internal/webtools`、图像/Files、四 preset、PTC/workflow/structured、子代理、goal、schedule/jobs、MCP 与终端均有实现；缺组合后的真实同输入差分与可复审交付记录。不能拿任一包全绿替代这些组合验收。
- **依赖**：SCOR-020 和相应 GAP 的具体修复；真实可用的图像模型、搜索/抓取服务、所选 provider；GAP-040 的平台结果。
- **验收 / 测试类型**：分层 live 场景：图片标准化→请求变体→Files复用→模型识图；搜索引用与抓取错误；四 preset 文本/文件/工具→child/workflow structured→job/goal/schedule→取消恢复→日志导出。已未实现的 GAP 应明确跳过并保留 open，不能整体标“全部完成”。

## 范围排除和待补证项目

- 用户已排除第三方 Harness 插件、插件市场/安装/开发、Creator、任意 Cordis/React 扩展兼容。官方内置用户功能即使源代码称为 plugin，也仍按上文逐项移植。
- Desktop-only product telemetry/update、默认 Web 不启用的 sidebar browser，不直接加入默认 Web 缺失总数；若其某个共享操作已被默认 Web 明确启用，须另以配置证据拆分。
- 没有从固定默认组合证明 LSP/tool-lsp 必须启用，故不作为确认缺失。自定义 profile 的 POSIX SSH execution providers 同样不自动扩大成默认 Web 必做，见 GAP-022。
- 官方 MCP 桥接器也将 audio/embedded resource 投影成明确占位文字并为 programmatic callers 保留 canonical value（`packages/mcp/mcp-client/src/tools.ts`）；不能只因 MCP 标准存在某能力，就把所有扩展协议、prompt templates 或资源订阅新增为本次官方 Web 必做。
- 官方本地 attachment GC/自动清理不作为已实现能力的缺口；远端 Files quota recovery 是 GAP-008。没有补证的通用 Session 自动保留设置也不冒称已存在。
- 本表不把真实服务未验证、功能部分实现和代码错误混计。关闭 GAP 需要同时给出实现入口、适用配置、针对性测试、实际结果及仍未验证的平台/服务；单纯修改文档状态不能关闭条目。

## 建议实施顺序与关闭记录

1. 先完成本轮 SCOR 联合回归，并保留已通过 SCOR-020 局部差分的记录，固定输入/上下文/图片投影基础。
2. 优先处理默认模型与文件链：GAP-001～010、014～018、035～038；其中有些可以独立并行，但共用路径/credentials/context surface 必须明确所有权。
3. 用 GAP-011/012、019～021、023～034 完成终端可操作流程，不以“图形界面不可复制”整体跳过。
4. 按明确配置与目标平台完成 GAP-013/016/022/039，随后以 GAP-040～042 形成真实环境验收记录。

后续每项至少记录：`GAP-ID / 适用配置与平台 / 实现提交 / 测试命令或 live 场景 / 结果与证据位置 / 未验证范围 / 状态更新日期`。本文件是可继续核销的差距清单，不是已经实现所有官方 Web 能力的交付声明。
