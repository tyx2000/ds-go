# 隔离复现材料

目标源码为 `069b4f63dfed211bcf97ae8fbc2929d109675dd1`。三个 `.go.txt`
文件仅为审查材料，不参与项目编译，也没有修改生产代码。

这些测试断言正确行为，在上述固定审查提交运行会失败，表示复现了缺陷。它们不是已经通过的修复测试。
附件、配置、会话和模拟模型均使用临时目录/合成内容，不访问真实模型服务。

在项目根目录执行：

```sh
review_root=$(mktemp -d /private/tmp/dsgo-review-069b4f6-XXXXXX)
git archive 069b4f63dfed211bcf97ae8fbc2929d109675dd1 | tar -x -C "$review_root"
cp docs/reviews/2026-10-29/cli-repro.go.txt "$review_root/internal/cli/review_069b4f6_test.go"
cp docs/reviews/2026-10-29/agent-repro.go.txt "$review_root/internal/agent/review_069b4f6_test.go"
cp docs/reviews/2026-10-29/model-repro.go.txt "$review_root/internal/model/review_069b4f6_test.go"
cd "$review_root"
GOCACHE=/private/tmp/dsgo-go-cache GOMODCACHE=/private/tmp/dsgo-go-mod GOPROXY=off \
  go test ./internal/cli ./internal/agent ./internal/model -run '^TestReview' -count=1 -timeout=30s
```

预期观察：

- CLI：待发附件跨会话、相对附件忽略工作目录、quoted 文件查询返回会话、数字自定义答案被拒、JSON chat 混入普通输出、压缩后指令不恢复。
- Agent：截断/空白/变大摘要被提交、压缩后工具结果无法剪枝、摘要请求丢掉实际 image block、可选摘要失败中断正常请求、同一步溢出重试耗尽步数。
- Model：可恢复的 HTTP 输入过长错误变成 `INVALID_REQUEST`。

`/attach FIFO` 的实际二进制复现另由主审查运行：在临时工作区创建 FIFO，向
`dsgo --home <temp> --cwd <temp> chat` 输入 `/attach log.txt`；两秒内不退出，由
Python `subprocess.run(..., timeout=2)` 终止并等待该隔离进程。该检查不应直接在日常会话中执行。

尚未执行运行复现的发现明确记录在主报告中，不因本目录存在而视为已经测试。

本轮修复、永久回归和联合结果见 [修复交接](fix-results.md)；未完整移植能力见
[42 项差距清单](remaining-capabilities.md)。旧复现材料只应放入上面的固定提交副本：
修复后附件草稿等内部 API 已变化，不要将旧 `.go.txt` 直接复制到当前源码树。
