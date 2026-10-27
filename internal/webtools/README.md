# 官方 Web 工具的 Go 移植

模型工具沿用 `web_search({queries: [...]})` 与 `web_fetch({url: ...})`。默认选择 DeepSeek Messages 搜索与匿名 HTTP 抓取；`UseDirectAPI` 同时切换到官方 Search/Fetch API。没有接入其他搜索供应商或添加浏览器自动化能力。

Messages 搜索使用独立的 `https://api.deepseek.com/anthropic/v1/messages`，`deepseek-v4-flash`、4096 max tokens、5 次 native search、禁用 thinking。仅解析结构化 `web_search_tool_result`，引用片段从 text citations 关联，不从模型回答抓链接。多查询去重后并发执行，任一失败取消并等待全部请求，成功结果按排名轮转合并、按 URL 去重，最多 8 个来源。日志保留无凭据的辅助模型请求。

直接 API 使用 `https://api.deepseek.com/v1/tools/search` 和 `/fetch`。搜索发送 query、较小的 count、need_content:false，可发送压缩空白并限制到128字符的原始问题。fetch 保留原 Markdown，报告原 URL 和 HTTP200，错误状态视为 retrieval failure。服务端搜索响应没有结果块时明确报错，不降级到其他服务。

API key 在 Messages 路由发送 x-api-key 与 Bearer，在 direct API 仅发送 Bearer。账户 token 只发送 x-dsh-auth-token，且固定 token 只允许配置的可信 origin（默认 api.deepseek.com）；动态 resolver 必须自行按账户授权 origin 校验。所有带鉴权 API 请求拒绝重定向。匿名 HTTP 使用独立 transport，无 Cookie/鉴权/环境代理，逐连接解析并检查所有 DNS 地址，连接 pin 到获准公共地址；只跟随5次同源重定向。响应上限5MB，正文10万字符，完整输出20万字符。

HTML 使用 Go 官方 x/net/html 解析，移除 active/hidden 内容，渲染常见 Markdown、表格和删除线，设置512层词法/树深度限制。此实现尚未做与原 Turndown 的全部渲染差分，少数复杂列表/表格/畸形HTML的空白和排版可能不同。目前正文解码支持 UTF-8、ASCII、Latin-1 与 Windows-1252；其他 charset 明确失败，尚未达到官方 TextDecoder 的完整字符集覆盖。

`Options` 可设置 SearchBaseURL（独立于对话模型 URL）、SearchModel、SearchMaxTokens、SearchMaxUses、SearchMaxResults、SearchMaxQueries、DirectAPIBaseURL、DirectAPICount、UseDirectAPI、OriginalQuestion、OmitOriginalQuestion、TokenOrigin 和 ResolveToken。CLI 是否已暴露所有设置需在功能矩阵另行记录。网络单测通过注入 RoundTripper 验证协议，不消耗真实 API；尚未有真实账户或 key 的端到端验证。

依据固定官方提交：

- [模型侧工具](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/web/tool-web/README.md)
- [Messages 搜索](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/web/web-search-deepseek/README.md)
- [Direct Search/Fetch](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/web/web-deepseek-api/README.md)
- [匿名 HTTP 抓取](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/web/web-fetch-http/README.md)
