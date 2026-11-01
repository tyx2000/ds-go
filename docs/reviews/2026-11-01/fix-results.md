# SCOR-027～033 修复与自检

目标：`b56eb33ce762c15dd722e7dbb3963436c4e1e4ea` 上的未提交修复。用户已授权修复；当前 standard/native 编码助手范围保持不变，不恢复已裁剪模块。原审查与修复前的失败材料保留在 [主报告](../../REVIEW-2026-11-01.zh-CN.md) 和 [复现说明](README.md)。

结案：**SCOR-027～033 全部 outcome: implemented、verification_result: passed。** 同一原始 7 项失败复现未修改断言，在最终冻结修复快照全部通过；完整 race、vet、固定输入校验、格式检查、编译、真实二进制冒烟及安装均已完成。基线失败材料原样保留，不以最终通过结果覆盖历史证据。

同日后续版本更新：按用户要求，显示版本增加北京时间 `YYYYMMDD.HHMMSS` 后缀，`make build/install` 自动注入构建时间，直接 Go 构建使用源码记录的更新时间。下方冻结快照及 `final-*` 安装/冒烟收据保留本轮修复首次验收时的历史记录；版本显示改动后的检查见 [版本验证记录](version-update.txt) 和 [版本冒烟记录](version-smoke.json)。

## SCOR-027 — 自动交互接管输入

- severity: P2；decision: adjusted；outcome: implemented。
- issue_evidence: confirmed；原 chat/io.Pipe + manager wake 复现问题未显示，问题发生在 composer 持锁等待输入与交互取锁之间。
- change: 以单一物理 stdin reader 和串行交互 lease 替代长时间持有 inputMu；composer 可挂起并保留草稿，问答组/审批独占逻辑输入；取消 private read 后，旧行仍必须丢弃至换行，不能当成后续答案。
- solution_fit: suited=yes，沿用现有 broker 与 LineEditor；executable=yes；affordable=yes；cost=medium，涉及输入所有权及 TTY 恢复；patch_scope: medium。
- solution_reliability: high（本机已验证范围），以 pipe、真实 PTY、UTF-8/粘贴/取消/迟到答案回归约束交接边界。
- verification_result: passed；原始 AutomaticQuestion 复现、定向交互回归及最终全量 race 通过。
- self_review: 已修复分页器与 stdin broker 竞争；已修复取消发生在 prompt 展示和 private read 注册之间时的迟到答案隔离；真实 PTY 读取及退出回归已通过，不以 bytes.Buffer 的行编辑测试替代终端验收。
- remaining_risk: 普通终端的读取本身可能等待外部输入；必须确保只保留一个物理 reader，并且它不持有会阻断回合取消的业务锁。

## SCOR-028 — 自动根回合失败传播

- severity: P2；decision: accepted；outcome: implemented。
- issue_evidence: confirmed；原模型失败已持久记录 failed turn，但 one-shot settle 返回 nil。
- change: manager 记录根自动 wake 的未恢复失败；one-shot 在受管工作安静后检查 root outcome。只有真正后续成功的根 turn 才能清理先前失败，nil/no-op wake 不能冒充恢复；子代理失败仍可交给父模型处理。
- solution_fit: suited=yes，按现有 root/wake 生命周期归属；executable=yes；affordable=yes；cost=medium；patch_scope: medium。
- solution_reliability: high（本机已验证范围），重点验证失败范围、恢复顺序和取消；不引入新的用户持久化格式。
- verification_result: passed；原始 OneShotSettlement 复现、自动根失败/恢复边界回归及最终全量 race 通过。
- self_review: 存储读取或恢复证据不足时采用保守失败锚点，明确精确的 failure 恢复边界，避免将无证据的状态当作恢复；settle 在返回前再核查受管工作状态，防止旧快照漏掉新激活的工作或失败。

## SCOR-029 — 超时和完成之间的原子交接

- severity: P2；decision: accepted；outcome: implemented。
- issue_evidence: confirmed；真实短命令的受控时序曾返回 promoted、完成输出已存在，但无 completion notice。
- change: timeout 分支在 job 锁内确认结算状态；已结束则交付 foreground 终态，否则原子退去前台 waiter 后转后台。相关 wait 的退出也需保证“通知已抑制”对应实际结果交付。
- solution_fit: suited=yes，局限在 waiter/settlement 边界；executable=yes；affordable=yes；cost=low/medium；patch_scope: small。
- solution_reliability: high，有明确交错及可控回归，不以压力测试未触发当证明。
- verification_result: passed；原始 Promoted 受控竞争复现、交接边界回归及最终全量 race 通过，未用未触发竞争的压力测试替代边界断言。

## SCOR-030 — 两代输入的 Markdown 导出

- severity: P2；decision: accepted；outcome: implemented。
- issue_evidence: confirmed；AppendInputBatch 的 direct 任务在旧 Markdown 分支缺失。
- change: 导出使用 core.UserInputMessages，旧 user/message 与新 user/input 均被展开；captured context 单独标注且保留全文，不冒充用户新写的任务。JSON/JSONL 格式及原始事件不改。
- solution_fit: suited=yes，复用输入投影 helper；executable=yes；affordable=yes；cost=low；patch_scope: small。
- solution_reliability: high，原复现和双代输入/审计不变断言可直接覆盖。
- verification_result: passed；原始 MarkdownExport 复现、`TestMarkdownExportKeepsBatchedUserTask`、`TestMarkdownExportPreservesBothInputGenerationsAndRawEvents` 及最终全量 race 通过。

## SCOR-031 — 摘要输出预算与上下文 headroom 分离

- severity: P2；decision: accepted；outcome: implemented。
- issue_evidence: confirmed；旧复现实际请求 `[agent:4096 compaction:65536]`。
- change: 摘要优先使用已路由请求预算；手动压缩从最近 request header 恢复预算，无 header 时用引擎配置。已知路由上限进一步约束预算；独立默认 ceiling 仅作回退。上下文 headroom/压力公式保持独立。
- solution_fit: suited=yes，沿用 Request/ContextRoute；executable=yes；affordable=yes；cost=low；patch_scope: small。
- solution_reliability: high，手动、主动 pressure 和 overflow 三条真实引擎路径验证；保留截断/非缩小摘要拒绝。
- verification_result: passed；原始 SummaryHonors 复现、`TestCompactionHonorsConfiguredOutputBudget`、`TestCompactionOutputBudgetUsesKnownRouteLimits` 及最终全量 race 通过，覆盖合法小预算、目录上限、引擎默认和手动 header。测试使用的默认预算预期已按 New 的既有 16,384 修正，不改变生产默认来迁就测试；原始复现的断言保持不变。
- self_review: 上下文 headroom 与摘要 output budget 已分离；合法的小输出预算不会再被硬编码 headroom 当作输出上限覆盖，截断和非缩小摘要保护仍保留。
- remaining_risk: 未知 provider 上限仍依赖用户配置的合法预算，不宣称拥有所有提供商能力目录。

## SCOR-032 — Skills 目录/显示/加载的一致来源

- severity: P2；decision: accepted；outcome: implemented。
- issue_evidence: confirmed；child-only 可由模型读取，但旧系统目录与 /skills 不可见。
- change: 统一 skillsFor，从 session 当前工作目录发现；systemFor、CLI 列表/正文与模型 load_skill 复用它。目录丢失按既有机制恢复；无 store 的嵌入调用保留合理 fallback。Session.CWD 仍是权限边界。
- solution_fit: suited=yes，复用 ensureWorkingDirectory；executable=yes；affordable=yes；cost=low/medium；patch_scope: small。
- solution_reliability: high，新增 child-only、同名覆盖、CLI/model 相同正文及目录恢复回归，不新增 watcher 或插件功能。
- verification_result: passed；原始 SkillCatalog 复现、目录切换/同名覆盖/目录恢复回归及最终 CLI 全量 race 均通过。

## SCOR-033 — 配置成功确认遵守 NDJSON

- severity: P2；decision: accepted；outcome: implemented。
- issue_evidence: confirmed；真实 Main 保存配置后输出 `Saved max_steps.`。
- change: 确认走现有 a.line，JSON 模式包装成单行记录，普通模式保留可读文本；失败不输出成功确认。
- solution_fit: suited=yes；executable=yes；affordable=yes；cost=low；patch_scope: tiny。
- solution_reliability: high，直接检查 Main 输出与保存值，并验证失败不改变先前值。
- verification_result: passed；原始 JSONConfigSet 复现、CLI 全量 race 和真实二进制配置冒烟均通过。成功确认逐行为合法 JSON，值 200 确已保存；非法 -1 退出 1、无 Saved 确认且原值 200 保持。

## 文档与旧测试整理

已将 internal/tools/README.md 和 managed_env.md 更新为当前 fresh bash/native 工具范围，移除对已删 API 的有效承诺；根 README 的 /todo TEXT 改为无参 /todo。旧提示样式测试显式设置 TERM，并新增 dumb 分支，保留 NO_COLOR 行为。

## 联合验证与最终自审

- 原始复现：相同的 7 项失败复现与断言在最终快照 `/private/tmp/dsgo-postcor-b56eb33-60svk5rg` 全部通过；[完整输出](original-repros-after.txt)。原始 [reproduction.txt](reproduction.txt) 和 [baseline-tests.txt](baseline-tests.txt) 未覆盖。
- 全量验证：`TERM=dumb NO_COLOR=1 go test -race ./... -count=1 -timeout=60s` 全绿，CLI 42.277s；[完整日志](final-race.txt)。`go vet ./...`、`python3 scripts/verify-upstream.py` 六份校验、`gofmt -l` 空输出、`git diff --check` 及 `go build -trimpath -o bin/dsgo ./cmd/dsgo` 通过。最终 25 个修改或新增 Go 文件与上述原始复现重跑快照逐字节一致。
- 实际冒烟：隔离临时 home/workspace、清除提供商环境，执行版本、NDJSON 配置成功/非法值、chat 帮助/历史/退出、无 writer 的 FIFO 附件拒绝及后续交互共 5 次真实二进制调用；全部通过，未调用模型。[完整记录](final-smoke.json)。
- 安装：已更新 `/Users/adib/.local/bin/dsgo`，PATH 指向该位置；与 `bin/dsgo` 的 SHA256 一致，为 `31b5b52d55c6ea79c79f6f3016c82b2627fa29f3a6e0fba3cd6c0507abd1866b`。版本 `0.1.0-dev`，upstream `687f282e8625c6b8d11ff58211a90524be2e4317`。[安装记录](final-install.json)。
- 边界：本轮未验证真实 provider、账户、付费模型或 Linux/Windows 实机。结案覆盖本机冻结快照的 7 项修复及自审边界，不声称所有平台、服务或全部保留能力已获得完整验收，也不恢复裁剪掉的 PTC、日程、工作流、终端、预览、present 或插件能力。
