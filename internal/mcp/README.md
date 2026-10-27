# MCP 移植状态

此包实现既有 MCP 服务器的连接与调用，不提供服务器安装或 Harness 插件加载。配置显式提供 stdio 命令/argv/env/cwd，或 Streamable HTTP URL/headers。调用方负责在启动前判断该配置是否已授权，并将 tools/call 包在统一审批、事件记录及取消流程中。

支持初始化、能力协商、tools 分页发现与调用、resources 发现/模板/读取、JSON-RPC 错误与取消通知，stdio ping/roots 回调、进程组清理，以及 HTTP JSON/SSE POST、session header 与 DELETE。MCP 原始结果保留为 JSON；模型上下文必须另做安全的内容投影，不能直接把 base64 写入历史。工具命名沿用官方 v1 算法。

当前协议实现为 2025-03-26；stdio 接受 2024-11-05 回退。固定官方版本首选 2026-07-28，因此现代订阅/任务语义尚未对齐。目前也没有自动重连、输出 JSON Schema 验证、HTTP GET/SSE 断线恢复或远端 OAuth 流程。标准调用默认 60 秒；帧限制 8 MiB，分页最多 100 页；这些资源上限也应纳入后续行为差分。

官方版本不支持 MCP prompt templates，本包没有添加该产品入口。官方资源订阅同样不在当前产品范围内。stdio 启动会删除环境变量名中包含 KEY/PASSWORD/SECRET/TOKEN 或以 DSH_/DSGO_ 开头的值，再合并显式 env；stderr 只保留最后 32 KiB。

已用独立子进程 stub 验证握手、分页、并发 ID 路由、取消、凭据环境清理；用本地 HTTP stub 验证 JSON/SSE、通知、session headers 和 DELETE。尚未连接真实第三方服务器，也不宣称完成 MCP 全部官方行为对齐。

核对依据：

- [固定官方 MCP 客户端说明](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/mcp/mcp-client/README.md)
- [固定官方工具命名实现](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/mcp/mcp-client/src/tools.ts)
- [MCP 2025-03-26 transport](https://modelcontextprotocol.io/specification/2025-03-26/basic/transports)
- [MCP 生命周期](https://modelcontextprotocol.io/specification/2025-03-26/basic/lifecycle)
- [MCP 取消](https://modelcontextprotocol.io/specification/2025-03-26/basic/utilities/cancellation)
