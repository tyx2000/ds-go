# 裁剪后闭环审查的隔离复现

原审查目标：`b56eb33ce762c15dd722e7dbb3963436c4e1e4ea`。主报告为 [REVIEW-2026-11-01](../../REVIEW-2026-11-01.zh-CN.md)，逐项决策见 [fix-results.md](fix-results.md)。**SCOR-027～033 已全部修复，原始 7 项失败复现以相同断言在最终修复快照全部通过；全量验证、真实二进制冒烟及安装均已完成。** `.go.txt` 保留原基线复现材料，不作为生产源码；下方基线复现命令仍针对原提交，不能将基线和修复快照的结果混读。

`baseline-tests.txt`：原有全量 race 测试在 `TERM=xterm-256color` 下通过。首次以宿主 `TERM=dumb` 执行时，原有 `TestPromptIsHighlightedOnlyOnInteractiveTerminals` 失败；该测试清空 NO_COLOR，却未设置 TERM。受控重跑通过，不把它混作裁剪引入的功能错误。`go vet ./...` 和六份固定官方输入校验也通过。旧 `/private/tmp` 模块缓存已清空，本轮使用现有用户模块缓存离线执行，没有更新依赖。

`reproduction.txt`：7 项“正确行为应该成立”的新增断言全部失败，表示复现缺陷，**不是修复后测试通过**。使用临时数据根、合成模型/输入；jobs 竞争仅执行 `sleep 0.03; printf completed`，不访问真实模型或账户。

以上两份历史记录原样保留。修复后的证据另存如下：

- [original-repros-after.txt](original-repros-after.txt)：相同原始 7 项复现未修改断言，在冻结快照 `/private/tmp/dsgo-postcor-b56eb33-60svk5rg` 全部通过。
- [final-race.txt](final-race.txt)：最终 `TERM=dumb NO_COLOR=1 go test -race ./... -count=1 -timeout=60s` 全部通过，CLI 42.277s。最终 25 个修改或新增 Go 文件与原始复现重跑快照逐字节一致。
- [final-smoke.json](final-smoke.json)：5 次真实二进制调用通过，覆盖版本、NDJSON 配置保存/非法值不改旧值、chat 帮助/历史/退出、无 writer 的 FIFO 附件立即拒绝及后续交互。使用隔离 home/workspace，清除提供商环境，未调用真实模型。
- [final-install.json](final-install.json)：已安装到 `/Users/adib/.local/bin/dsgo`，PATH 指向该位置。安装件与 `bin/dsgo` 的 SHA256 均为 `31b5b52d55c6ea79c79f6f3016c82b2627fa29f3a6e0fba3cd6c0507abd1866b`；版本 `0.1.0-dev`，upstream `687f282e8625c6b8d11ff58211a90524be2e4317`。

最终 `go vet ./...`、`python3 scripts/verify-upstream.py` 六份输入校验、`gofmt -l` 空输出、`git diff --check` 和 `go build -trimpath -o bin/dsgo ./cmd/dsgo` 也通过。提示样式 fixture 已固定 TERM 并补 dumb 分支；原环境失败证据没有删除。真实 provider/account、付费模型与 Linux/Windows 实机仍未验证；本轮只结案裁剪后范围内的 7 项问题及相关自审边界，不恢复已裁剪能力。

重现原审查基线失败时，在本项目根目录建立隔离副本：

```sh
review_dir=$(mktemp -d /private/tmp/dsgo-review-b56eb33-XXXXXX)
git archive b56eb33ce762c15dd722e7dbb3963436c4e1e4ea | tar -x -C "$review_dir"
cp docs/reviews/2026-11-01/agent-repro.go.txt "$review_dir/internal/agent/review_b56eb33_test.go"
cp docs/reviews/2026-11-01/cli-repro.go.txt "$review_dir/internal/cli/review_b56eb33_test.go"
cp docs/reviews/2026-11-01/state-repro.go.txt "$review_dir/internal/state/review_b56eb33_test.go"
cp docs/reviews/2026-11-01/tools-repro.go.txt "$review_dir/internal/tools/review_b56eb33_test.go"
cp docs/reviews/2026-11-01/lifecycle-repro.go.txt "$review_dir/internal/cli/review_trim_lifecycle_test.go"
cd "$review_dir"
TERM=xterm-256color GOCACHE=/private/tmp/dsgo-go-cache \
  GOMODCACHE=/Users/adib/go/pkg/mod GOPROXY=off \
  go test -race ./internal/cli ./internal/agent ./internal/state ./internal/tools \
  -run '^TestReview(SkillCatalog|AutomaticQuestion|OneShotSettlement|JSONConfigSet|MarkdownExport|SummaryHonors|Promoted)' \
  -count=1 -timeout=15s
```

其他机器请使用其已有模块缓存，或先按 go.sum 获取依赖。原有套件中的本机 HTTP/PTY/Seatbelt 用例需要相应宿主权限。

报告映射：SCOR-027 为 AutomaticQuestion；028 为 OneShotSettlement；029 为 Promoted；030 为 MarkdownExport；031 为 SummaryHonors；032 为 SkillCatalog；033 为 JSONConfigSet。

另一个候选“双锁反序”已被真实 Inbox 路径的受控测试反证，未计入发现：`user/inbox` 不被 `core.UserInputMessages` 识别，不会在该通知中重置 jobs wake budget。不要根据早期候选描述修复不存在的锁循环。
