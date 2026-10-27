# DeepSeek Harness 转为 Go 终端工具的可行性初评

初评日期：2026-10-23；范围修订日期：2026-10-26（Asia/Shanghai）。官方仓库：`deepseek-ai/deepseek-harness`。技术分析沿用已检查的提交 `687f282e8625c6b8d11ff58211a90524be2e4317`，提交时间为 2026-10-17；本次修订不代表重新审计了最新版本的全部功能。

这是基于官方架构、功能文档和关键接口源码的静态评估，尚未构建运行、调用真实模型或完成全仓逐文件审计。

实施状态更新（2026-10-26）：已按用户指令开始 Go 实现，启动命令 `dsgo`。本文件保留初评时点的结论；当前代码、验证和未完成项见 [README](README.md) 与 [功能对照表](docs/PARITY.md)。已完成本地替身协议与核心运行测试，尚未使用真实账户/模型验证，不代表完整性验收通过。

## 结论

目标是用 Go 实现官方 Web 版现有使用功能的完整终端版本。按这个有限范围评估，项目可行；无需为任意第三方插件或未来扩展提供兼容平台。

- 最终交付按 **Go 代理核心 + Go CLI/TUI + 官方内置能力的实现**规划，不能仅翻译现有 headless 入口后认定完成。
- 官方功能内部由 Cordis 插件实现，属于原项目的组织方式。移植时可以改成固定的 Go 模块，不必复制通用插件加载、安装、热更新和生态兼容接口。
- 官方 Code Mode/PTC 使用的代码执行环境需单独处理。“使用 Go 实现应用”不自动等于“禁止 Node 或其他工具依赖”；用户尚未提出零 Node 或单一静态二进制要求。
- 完整性以官方 Web 功能清单逐项验收，图形功能也需要列出终端对应交互，不能用省略或文本摘要直接替代完整功能。

复用官方 Host 的 Go 终端前端可以作为验证或过渡方案；它与 Go 核心移植属于不同交付范围。

## 明确范围

纳入官方 Web 版的对话、模型与凭据配置、会话操作、文件及代码操作、工具执行、权限审批、子代理、任务、官方执行模式，以及文件和产物交互等使用功能。最终清单应从固定版本实际 Web 组合和官方内置界面逐项核对。

排除插件市场、插件安装与开发、Creator 中用于扩展插件的流程、第三方或社区插件、自定义 React UI 兼容、任意动态 Cordis 配置兼容，以及额外加入的非官方能力。官方功能内部依赖的模块仍需保留或等价实现。

官方自带的 Skills、MCP 等若属于所选 Web 基线中的内置使用功能，保留其官方已有入口和行为；不因此扩展范围去安装、移植或承诺兼容任意第三方扩展。官方可选或实验功能需在清单中标明是否随目标版本提供，不以“非默认启用”为由自动排除。

## 官方目前已经提供什么

官方不仅有 Web：内置 `web`、`headless`、`sdk`、`sdk-minimal`、`acp` profile。`headless` 可从终端执行一次任务并退出；SDK 和 ACP 可供其他程序驱动。文档里的 `tui --resume` 是假设已安装自定义终端 profile 的示例，不能据此认定已内置完整交互 TUI。[官方 CLI 说明](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/apps/cli/README.md)

`web`、`headless`、`sdk`、`acp` 复用 `dsh-base`，但上层组合及默认行为不完全相同；`sdk-minimal` 使用独立最小组合。因此“共用核心”不代表换一个 profile 就自动获得所有 Web 能力。[共享基础组合](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/bundle/base/README.md)

## 功能映射与迁移难度

下表区分“可在终端表达”与“已经由现有协议完整开放”。前者可行并不意味着后者无需开发。

| 功能范围 | 终端中的承载方式 | Go 移植的主要工作 |
| --- | --- | --- |
| 多轮对话、推理/回答流、模型与推理强度选择 | 对话 TUI、状态栏、命令参数 | 对齐模型适配、流解析、重试、取消和请求历史 |
| 文件读写编辑、搜索、diff、Shell、PTY、LSP | 文件视图、diff 视图、工具记录、终端面板 | 保留路径、进程、输出截断及取消语义 |
| Skills、MCP、计划、Todo、Goal | 命令、列表、进度面板 | 保留作用域、提示词、事件和生命周期 |
| 会话历史、分叉、恢复、导出、上下文压缩 | 会话列表、时间线、导出命令 | 重建事件日志、历史迁移和模型上下文投影 |
| 子代理及目标版本随附的官方团队/工作流能力 | 任务树、消息视图、并发状态 | 父子所有权、消息路由、完成等待和资源清理 |
| 审批、权限模式、沙箱 | 审批队列、键盘确认、审计记录 | 实现同等策略和各操作系统后端 |
| 后台 jobs、定时任务 | jobs/schedule 命令与常驻 Host | 区分进程内任务与持久计划，保持重启行为 |
| 官方 Code Mode/PTC 等执行模式 | 代码与执行记录视图 | 保留执行语言、工具绑定、超时、状态和取消行为 |
| HTML/PDF/Office/表格/图片预览 | 文本摘要、文件路径、外部查看器或可选图形预览 | 任意终端无法无损承载原网页交互 |

这是迁移范围框架，不是穷尽清单。正式盘点从固定版本的官方 Web 产品出发，不能以现有 headless 功能作为上限，也不能把任意仓库包或社区插件自动算入目标。

## 不能低估的差异

**固定模块的行为对齐。** 原项目把模型、循环、日志和工具组织成 Cordis 插件。Go 版本无需兼容 Cordis 的通用扩展接口，但需要保留这些官方内置模块之间的调用顺序、作用域、取消和资源清理行为。这是业务语义移植问题，不能再把第三方插件兼容作为项目阻碍。[架构](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/docs/architecture.md)

**Code Mode / PTC。** 当前运行时执行模型生成的 TypeScript，允许受策略约束的 Node 文件、网络及子进程操作，并支持每次新进程和保留内存的 Node REPL 两种模式。替换成“执行 Go 代码”会改变工具和提示词契约；仅嵌入普通 JavaScript 解释器，也不能据此认定兼容 Node。[PTC 运行时](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/ptc-runtime/ptc-runtime-node/README.md)

**模型协议。** 该版本官方 DeepSeek 适配器使用 Messages 协议及 Files 能力，涉及推理内容回放、图片处理、鉴权和请求配置。只接一个 OpenAI-compatible Chat Completions 客户端不足以证明请求行为一致。[DeepSeek 适配器](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/llm/llm-deepseek/README.md)

**会话与恢复。** 官方以追加式事件日志派生模型历史，区分已提交消息、失败/取消尝试和瞬时流；分叉、恢复及导出共同依赖这些事件。Go 不能只保存聊天 `messages[]`。是否直接读写官方历史文件属于另一个数据兼容要求，不自动纳入功能对等；若提供导入或共享存储，则必须单独验证格式迁移及写入所有权。[会话架构](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/docs/architecture.md)

**审批与沙箱。** 需要保留取消、拒绝、审批不可用、迟到响应、审计及执行顺序。沙箱还涉及 Linux、macOS、Windows 等不同机制，不能用路径前缀检查代替进程层约束。实现应保留官方实际限制及后端强度，不能将“移植官方策略”解释为更强的隔离保证。[审批](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/docs/subsystems/approval.md)、[沙箱](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/docs/subsystems/sandbox.md)

**常驻生命周期。** 定时任务是 Host 级持久能力，当前依赖 Web Session controller 和持久投递确认，不能简单挂进一次性 headless/SDK 组合。完整终端产品需要常驻 Host/daemon；退出 TUI、停止 Host 和关闭会话必须具有明确且不同的效果。后台 jobs 也不能被误当作重启后自动恢复的持久进程。[定时任务](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/schedule/schedule/README.md)、[jobs](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/jobs/jobs/README.md)

**官方图形交互。** 只需针对官方现有预览设计有限数量的终端视图，不需要转换任意 React 插件。PDF、图片、表格和可执行 HTML 的实际操作要逐项列出；键盘操作可以替代鼠标，但文本摘要不等于完整预览。便携文本终端不能无损承载可执行 HTML；外部查看器属于可选方案，未经明确约定不能用它宣称已经满足完整终端交付。[文档预览](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/client/ui-sidebar-documentpreview/README.md)

## 可选技术路线

### A. Go 核心 + Go CLI/TUI：目标实现

Go 实现代理循环、模型适配、会话事件与持久化、内置工具、权限审批、任务管理和终端交互。按固定官方能力拆分内部模块，不提供插件 SDK、动态加载、市场或安装管理。官方代码执行环境等外部依赖单列，保持其行为而不强制把所有执行语言改成 Go。

可按 `agent`、`model`、`session`、`tools`、`permissions`、`tasks`、`tui`、`cli` 等职责组织代码；这是实现建议，不是要求复刻原项目目录。

### B. Go CLI/TUI + 官方 Host：验证或过渡方案

Go 提供终端交互，官方 Host 提供执行服务。用于验证终端映射及建立行为对照，不应将其直接标记为已完成 Go 核心移植。

官方 Connection 已为非浏览器 API 调用者提供认证 Peer、HTTP RPC 与 WebSocket Mux；人工交互还需要正确声明参与模式。Go 客户端要实现认证、流复用、双向问题/审批、取消、重连和二进制响应。也可利用载体无关的 RPC/Fetch 注册机制设计终端 bridge。[Connection 协议](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/client/connection/README.md)

不能仅套用现成 SDK 或 ACP 就声称完成：SDK 缺少会话级取消/关闭及完整人工交互；ACP 虽支持取消、关闭和权限请求，却明确省略 DSH 特有的卡片、计划、终端视图及部分会话操作。两者适合验证局部流程，不足以单独覆盖完整 Web 产品。[SDK 协议](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/sdk/protocol/README.md)、[ACP 范围](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/acp/acp/README.md)

这条路线保留完整官方运行时，仍需开发官方界面的终端对应操作，并核对 Web 导向提示词与终端上下文之间的差异。

### 不要求的工作

无需实现用于任意第三方插件的跨进程兼容宿主，无需复刻 Cordis 通用插件框架，无需移植社区功能。也没有“禁止一切 Node 依赖”或“所有未来版本永远兼容”的额外要求。

## 实施拆分与估算依据

范围已收敛为有限的官方功能，不能继续将插件生态工作量算进总工期。当前尚未完成逐项盘点和原型验证，不提供“全功能已可按固定周数完成”的承诺；局部原型的周期也不能替代完整交付工期。

实施顺序建议如下：

1. 固定官方 Web 版本，盘点全部使用入口、预设和后台服务，去除已明确排除的插件扩展流程。
2. 用一个纵向原型验证 Go 模型请求、工具循环、实时输出、审批、取消和会话恢复，并与官方行为比较。
3. 实现文件/Shell/PTY、会话管理、子代理、任务、配置等官方功能及对应 TUI。
4. 完成官方执行模式、产物与预览交互，以及所有尚未映射的官方页面功能。
5. 运行行为差分与目标平台验证，再形成完整性报告及实际交付估算。

排期的主要变量是官方功能数量、模型适配细节、平台范围、官方代码执行环境及预览交互，不再是任意插件兼容。

## “完全一致”如何验收

1. 固定官方提交、Web 内置功能和预设、模型路由及目标操作系统，建立“官方功能 → 官方接口/事件 → 终端操作 → 验收样例”清单。
2. 对每项能力标记行为保留、终端等价交互、待验证或缺失。未完成的官方功能不能计作已一致；外部图形承载必须单列，不能默认视作满足完整终端版本。
3. 使用确定性模型 stub 和固定工具结果做差分测试，比较模型请求、工具 schema、调用参数、持久事件、审批结果与状态变化；对时间戳及随机 ID 做受控归一化。
4. 覆盖取消、重试、流中断、崩溃恢复、分叉、上下文压缩、子代理清理及任务重启行为；若承诺导入原始历史数据，另验证其格式迁移。
5. 在各目标系统验证真实沙箱及 PTY；单个平台通过不代表其他平台也通过。
6. 真实模型用于端到端验证；不能要求随机模型回答逐字相同，也不能仅凭若干演示任务成功认定全功能对等。

正式实现前还应完成完整功能盘点、协议原型和依赖/许可证核对。官方仓库采用 MIT，但分发原代码、运行时及依赖时仍需核对相应声明。[仓库许可证](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/LICENSE)、[第三方声明](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/THIRD_PARTY_NOTICES.md)

评估结论：按官方 Web 使用功能的有限集合规划 Go 终端移植是可行的。实施现已开始；最终完整性交付仍取决于官方功能盘点、终端交互设计及逐项验收。
