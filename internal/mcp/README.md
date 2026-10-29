# MCP 移植状态

此包实现既有 MCP 服务器的连接与调用，不提供服务器安装或 Harness 插件加载。配置显式提供 stdio 命令/argv/env/cwd，或 Streamable HTTP URL/headers。调用方负责在启动前判断该配置是否已授权，并将 tools/call 包在统一审批、事件记录及取消流程中。

支持能力协商、tools 分页发现与调用、resources 发现/模板/读取、JSON-RPC 错误与取消通知、进程组清理，以及 HTTP JSON/SSE POST、session header 与 DELETE。MCP 原始结果保留为 JSON；模型上下文必须另做安全的内容投影，不能直接把 base64 写入历史。工具命名沿用官方 v1 算法。客户端声明空 capabilities，与固定官方桥接器相同；不会自行增加 roots、sampling 或 elicitation 处理器。

现代协议通过 `server/discover` 协商 `2026-07-28`。stdio 使用一次性 sibling 进程探测，丢弃无关消息且不向其回复，确认回收后才启动真正服务进程；探测退出/超时可降级。HTTP 在同一传输探测，认证错误、5xx、网络错误和超时不作为旧协议证据；`-32022` mutual-version corrective continuation 最多一次。降级后的 `initialize` 只提供和接受旧版本：`2025-11-25`、`2025-06-18`、`2025-03-26`、`2024-11-05`、`2024-10-07`。现代连接没有 `initialize`/`notifications/initialized`。先前仅在 initialize 里填写 2026 版本号的实现已被替换，不能当作现代协议对齐证据。

每个现代请求/通知携带 protocolVersion、clientInfo 和 clientCapabilities `_meta`；HTTP 请求的 `Mcp-Method`/`Mcp-Name` 与正文一致，不能直接表示的名称使用固定 SDK 的 Base64 sentinel 编码。`x-mcp-header` 声明按 properties 可达性、primitive 类型、RFC token 和忽略大小写唯一性检查；现代 HTTP 逐工具剔除无效声明，stdio 按 SDK 约定忽略。合法参数生成 `Mcp-Param-*`，含嵌套路径、Base64 sentinel 和 JavaScript primitive 数值规则；这些 header 跟随一次执行和显式 continuation，不通过全局可变状态共享。

现代结果必须有 `resultType`；`structuredContent` 可以是任意 JSON 值，旧协议仍要求对象。成功结果按所广告 output schema 验证；wire 合法但无法编译的 output schema 只让对应工具在远端执行前失败，不拒绝整组。input schema 保留固定 wire 形状约束；官方原始 MCP ToolDefinition 不预编译 parameters，也不以该 schema 在客户端拒绝模型参数，参数语义由服务器处理。要求 task 执行的旧协议工具明确拒绝，与官方桥接器一致。

`OpenSupervisor` 管理一个服务器的连接代次。初始连接失败默认记录错误并允许 Harness 启动；`FailOnStartupError` 可选择严格启动。重连默认从 500ms 指数退避到 30s、最多 10 次；连接稳定超过 30s 重置预算，短暂连接成功后再次崩溃仍消耗同一故障预算。断线期间保留旧工具和归属指令，调用明确失败；重连耗尽后清除。禁用重连则保留旧工具直至关闭或重新加载。每个新代次先等待旧传输确认关闭；确认失败时停止恢复，防止同时留下两个服务器进程。

工具列表变化会串行重新发现并发布完整工具组。重复名称、分页或 wire schema 等发现失败保留旧组；注册冲突回滚成该服务器零工具。`Publish` 回调由调用方原子替换服务器拥有的组，不能回调 supervisor。归属指令采用 `### MCP server: NAME` 前缀，前缀计入默认 32,768 UTF-8 字节上限。

旧连接支持可选 GET/SSE 通知流，405 表示不提供；现代连接只在服务器广告 `tools.listChanged` 时自动打开 `subscriptions/listen`。独立字符串 listen ID 与普通请求 ID 分离，等待带同一 subscriptionId 的 acknowledgement；无关/未被接受的通知不触发刷新。拒绝订阅不阻断连接，关闭订阅同时终止请求 stream 并发送 cancelled；远端关闭后不会自行重新 listen。

SSE 只派发含 data 字段且由空行完成的事件；合法空 data priming 可保存游标，纯 id 段不会派发。retry 字段独立生效，EOF 未完成事件丢弃；非 message 事件不交给 MCP 消息处理器。SSE 断流可带 `Last-Event-ID` 用 GET 恢复，不重新提交原 POST。恢复采用固定 SDK 默认的 1s、1.5 倍、失败重开最多 2 次，尊重有效 server retry。普通现代 HTTP 取消只中止该请求 stream；旧 HTTP 和 stdio 保留取消通知。单次 HTTP 请求错误和 SSE 恢复失败不会被偷偷变成新的连接代次；真正传输关闭驱动 supervisor。只有 initialize 响应可设置 HTTP session ID，discover 上的同名 header 被忽略。

标准工具和资源调用每 leg 默认 60 秒，可配置。分页最多 64 页；相同 items 且相同 nextCursor 视为无进展终止，其他未终止分页达到上限会失败且不缓存部分结果。stdio 仅限制未读输入缓冲为 10 MiB，输出没有另设帧限额。HTTP JSON/body 与 SSE 默认不设置该缓冲上限，与固定 SDK 及 lockfile 中的 eventsource-parser 3.1.0 一致。正常 stdio 关闭先给 EOF 清理 2 秒，再给 TERM 清理 2 秒；探测进程则直接 TERM、等待 1 秒。dsgo 在 KILL 后另等最多 1 秒确认回收，超时阻止新代次。非 JSON stdout 行跳过，schema 错误记录后继续读取，输入缓冲溢出关闭连接。官方 transport 配置仅传静态 headers，没有配置 OAuth provider；dsgo 同样没有添加额外登录流程。

MCP 显式 HTTP endpoint 使用 fetch 的重定向规则，最多 20 跳；301/302 的 POST 和 303 的非 GET/HEAD 改为 GET 并删除 body headers，307/308 保留 body。跨 scheme、host 或有效 port 时删除 Authorization、Proxy-Authorization、Cookie、Host；其余自定义及 MCP headers 按 fetch 继续传递，后续跳回原 origin 也不会恢复已剥离凭据。此行为仅用于 MCP transport。

默认缓存保存聚合后的 list 及按 URI 的 resources/read；TTL 默认 0、上限 24 小时，零 TTL 的 list 仍保留派生 schema，资源内容则不保存。资源缓存按固定 SDK 默认保留最多 512 个 URI、FIFO 淘汰，list 不受 URI 配额挤出。每个连接自己的 cache 不跨服务器、凭据配置或重连共享。list_changed 和 resources/updated 同步提升失效代数，防止中途失效的旧请求重新写回缓存；supervisor 的 tools 同步始终强制刷新。

默认 input_required 驱动只处理官方空 capabilities 下可执行的路径：requestState-only 使用新 request ID、原参数/meta、逐值精确回显和 250ms 节奏，最多 10 轮；取消中止等待。服务器要求 roots/sampling/elicitation 时按未注册 handler 失败，不增加 UI 能力，也不发新的工具请求。一般传输错误、HTTP/RPC 错误和未知 resultType 都不会触发 continuation；只有明确 input_required 才进入这条协议流程。

官方版本不支持 MCP prompt templates，本包没有添加该产品入口。官方资源订阅同样不在当前产品范围内。stdio 启动会删除环境变量名中包含 KEY/PASSWORD/SECRET/TOKEN 或以 DSH_/DSGO_ 开头的值，再合并显式 env；stderr 只保留最后 32 KiB。

回归入口包括独立子进程探测与全新服务管道、分页/并发 ID/取消/环境清理，以及 HTTP modern/legacy 黄金 wire、现代订阅、resultType、错误协商、游标恢复、session headers/DELETE、启动恢复、重连预算/耗尽/禁用、列表更新和归属指令限额。尚未连接真实第三方服务器。

默认 tools/list、tools/call、resources/list、templates/read 及连接结果的 wire 合同覆盖 content union、资源内容、icons、annotations、可选字段类型和两个 era 的 input/output schema 形状差异。178 条固定 corpus 已分别通过 exact SDK 2.2.0 + lockfile zod 4.4.3 的真实 schema 校验器与 Go 校验器；`testdata/check-sdk-wire.mjs` 仅重定向只读源码的 import 并剥离 TypeScript 类型，不重写 SDK schema。wire 的 atob 接受省略 padding、ASCII whitespace 和非零 pad bits；durable 图片准入继续按官方桥接器要求 canonical Base64，拒绝时保留程序访问的原始值并投影诊断文本。

已覆盖固定桥接器默认的 cache/input_required/header 路径，仍未完成所有 wire schema、运行时边界及真实服务器差分。自定义共享 cache store、多 principal、手工订阅和额外交互能力并未添加；官方桥接器没有提供这些用户配置。现代握手与上述默认路径的实现不等于 MCP 全部完成。

SDK 基线见 [sdk-baseline.json](sdk-baseline.json)：NPM `@modelcontextprotocol/client@2.2.0` tarball 已校验 SHA-512，来源证明指向源码提交 `dd22ba25ddbe7cd8822e1fe9c738a974996a28f6`。只读取其源码作 Go 行为移植，没有安装或执行 SDK/server；原始许可保存在 [SDK_LICENSE](SDK_LICENSE)。

核对依据：

- [固定官方 MCP 客户端说明](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/mcp/mcp-client/README.md)
- [固定官方工具命名实现](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/mcp/mcp-client/src/tools.ts)
- [固定官方连接监督器](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/mcp/mcp-client/src/connection.ts)
- [固定 SDK 协商实现](https://github.com/modelcontextprotocol/typescript-sdk/blob/dd22ba25ddbe7cd8822e1fe9c738a974996a28f6/packages/client/src/client/versionNegotiation.ts)
- [固定 SDK Client 与订阅](https://github.com/modelcontextprotocol/typescript-sdk/blob/dd22ba25ddbe7cd8822e1fe9c738a974996a28f6/packages/client/src/client/client.ts)
- [MCP 2025-03-26 transport](https://modelcontextprotocol.io/specification/2025-03-26/basic/transports)
- [MCP 生命周期](https://modelcontextprotocol.io/specification/2025-03-26/basic/lifecycle)
- [MCP 取消](https://modelcontextprotocol.io/specification/2025-03-26/basic/utilities/cancellation)
- [固定 SDK stdio 缓冲与关闭](https://github.com/modelcontextprotocol/typescript-sdk/blob/dd22ba25ddbe7cd8822e1fe9c738a974996a28f6/packages/client/src/client/stdio.ts)
- [固定官方依赖锁](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/pnpm-lock.yaml)
- [eventsource-parser 3.1.0 stream 默认](https://github.com/rexxars/eventsource-parser/blob/v3.1.0/src/stream.ts)
- [Node Undici fetch 重定向规则](https://github.com/nodejs/undici/blob/v7.16.0/lib/web/fetch/index.js)
