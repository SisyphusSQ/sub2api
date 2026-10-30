# TOO-674 开发与验证记录

日期：2026-10-30（Asia/Shanghai）。完整需求与详细开发计划以 [TOO-674](https://linear.app/sisyphus-sq/issue/TOO-674) 为准，本文记录实现决策和已有证据。

## 基线与交付状态

- 分支：`suqing/fix/image-token-billing`，从 `suqing/stable@84d10c31cb6db15bdf0eeb29091fb9b79ef939cb` 创建。
- 开发前已拉取上游 `main@d851a3847`；`openai_gateway_usage.go` 和 `billing_service.go` 相对 v0.2.19 无差异，没有发现等价修复。未整体合入上游开发分支。
- 当前实现保留在工作树，尚无本补丁的源码提交；提交后需将提交号补到本页与补丁清单。未推送、未合入 stable、未部署、未提上游 PR。
- 开发前已有的 `.gitignore` 修改用于保护私有测试基准，原样保留，不能算作本补丁改动。本补丁另新增一行验证文档白名单，避免本文被既有 `docs/local/*` 规则忽略。

## 原因与修复

`calculateOpenAIRecordUsageCost` 在有图片产出时，仅接受 `resolveOpenAIChannelPricing` 返回的显式 token 配置。该解析接口有意过滤默认价表，导致默认模型 token 价表永远无法进入既有 token 计算路径，最终可能使用来自 Gemini 的固定图片兜底价。

修复保留既有定价解析器和 `BillingService` 费用分桶算法，在没有显式价格时检查有效用量与默认 token 价表，再选择 token 路径。没有改写 `billing_service.go` 的公共计算逻辑，没有新增生产数据模型或数据库迁移。

同时修正两处会影响同一闭环的问题：

- 无 resolver 的旧 token 入口补写 `BillingModeToken`，避免 token 金额被显示为按张计费，并错误记录图片独立倍率。
- API Key Images 请求复用 OAuth 直连图片端点的用量解析；缺少输出分类时，Images 端点的输出 token 归为图片输出。非流式、SSE 和请求流式但上游返回 JSON 的情况均覆盖。流式合并复用现有 `mergeOpenAIUsageNonZero`，保留图片缓存及缓存写入字段。

图片专用解析只在 Images 路径使用；通用 Responses/Chat 的用量解析规则保持原有职责。显式输出分类（含零值和兼容协议的 completion 明细）优先，不以图片产出数量乘算整份 token 用量。

## 定价优先级

| 条件 | 行为 |
| --- | --- |
| 图片端点且分组开启 `image_count_billing` | 强制图片计费，沿用既有图片价格选择 |
| 未触发开关，命中分组或渠道 token 配置 | token 计费，保留已有高于普通尺寸覆盖价的优先级 |
| 命中分组模型按次/图片配置 | 使用分组模型价格 |
| 命中渠道按次/图片配置 | 普通分组尺寸覆盖价优先，否则渠道价格 |
| 没有上述显式模型配置，但有匹配尺寸的分组图片价 | 使用分组图片价，显式零价保留 |
| 没有显式覆盖，有有效 token 用量且候选模型有默认 token 价表 | token 计费，支持原有候选模型回退 |
| 缺少 token 用量或候选模型均无 token 价表 | 保留原有图片兜底契约；不生成假 token、不伪称免费 |

混合 Responses/Chat/WS 请求不受仅适用于图片端点的按张开关影响。未配置默认 token 价的历史图片兜底仍可能使用 `defaultImageGenerationPrice`；本补丁没有重新定义缺失价格场景的业务规则。

输入总 token 包含图片及缓存；普通输入先扣除缓存，再区分图片与文本。输出总 token 包含图片输出，文本输出扣除图片输出。价格以美元/token 计算；每百万 token 的显示价格仅换算一次。缓存图片占比仅采信明确明细，不根据总缓存数量推测。

## 开发验证

| 入口 | 结果 | 本机日志 |
| --- | --- | --- |
| 修复前默认价表回归 | 6 个子场景均失败：预期 token，实际 image，已重现原问题 | `/tmp/sub2api-too-674-before.log` |
| 定向计费、入账和既有图片回归 | 通过 | `/tmp/sub2api-too-674-targeted.log` |
| API Key 原生 JSON/SSE/流式 JSON、既有 ForwardImages 和 CodexDirectImages 回归 | 通过 | `/tmp/sub2api-too-674-native.log` |
| `go test -tags=unit ./...` | 通过，59 个有测试包、54 个无测试包；包含本机两个私有探针包。此轮全量在后续图片解析补充前启动，后续改动由下述最终定向测试覆盖 | `/tmp/sub2api-too-674-unit.log` |
| 最终图片相关定向 race | 通过，覆盖新增用例、已有原生/OAuth 图片转发及入账测试 | `/tmp/sub2api-too-674-final-race.log` |
| 最后新增的无分组及按张开关覆盖 token 卡用例 | 通过；4 个新增测试函数及全部子场景一并执行 | `/tmp/sub2api-too-674-final-added.log` |
| 最终源码静态检查 | 通过，`0 issues`；显式覆盖源码目录，未将私有 `backend/data` 探针计入检查结论 | `/tmp/sub2api-too-674-final-lint.log` |
| 后端开发构建 | 通过，版本标记 `dev-too-674`；用于隔离验收，不作为正式发版 | `/tmp/sub2api-too-674-build.log` |
| `gofmt`、`git diff --check` | 通过 | 会话工具输出 |

最终 race 入口：

```sh
cd /Users/suqing/Coding/golang/00_self/sub2api/backend
go test -race -p 4 -tags=unit ./internal/service -run 'Test(OpenAIImageTokenBilling|OpenAIGatewayServiceForwardImages|CodexDirectImages|OpenAIGatewayServiceRecordUsage_.*Image|CalculateOpenAIRecordUsageCost_.*Image)' -count=1
```

最终补充测试与静态检查入口：

```sh
go test -tags=unit ./internal/service -run '^TestOpenAIImageTokenBilling' -count=1
golangci-lint run ./cmd/... ./internal/... ./pkg/... ./migrations/... ./ent/...
```

回归矩阵覆盖 Flare、Sunburst、日期模型、有/无 resolver、无分组、图文及缓存分桶、显式分组/渠道 token 与图片价、按次零价、尺寸零价、尺寸未命中、按张开关与 token 卡冲突、混合请求、缺少用量/价格、候选回退、token/图片独立倍率，以及 `RecordUsage` 的模式、费用和资金命令。

## 真实隔离网关验收

使用 sqmc05 已有专用 PostgreSQL/Redis 测试副本及回环地址 `127.0.0.1:8085`，新建本卡专用用户、3 个分组、3 个账号副本及 Key；没有修改既有原生账号配置或访问 sqmc03 生产服务。

真实调用预算设为 USD 20、最多 12 次。实际发出 5 次上游 Images 请求：4 次成功、1 次因测试脚本上传的 MIME 为 `application/octet-stream` 被 400 `unsupported_file_mimetype` 拒绝。修正为 `image/png` 后完成编辑，拒绝请求没有新增平台账单。另有一次服务尚未监听时的本地连接失败；核对启动时间与零账单、零扣费后保留该次证据再继续，未重放已成功请求。

| 场景 | 文本输入 | 图片输入 | 图片输出 | 内部账单 USD | 模式 |
| --- | ---: | ---: | ---: | ---: | --- |
| Flare 生成 | 20 | 0 | 196 | 0.005980 | token |
| Flare 编辑 | 29 | 1024 | 196 | 0.014217 | token |
| Sunburst 生成 | 20 | 0 | 196 | 0.005980 | token |
| Flare 显式按张 | 20 | 0 | 196 | 0.123000 | image |

均使用明确的 `1024x1024`、`quality=low`、`n=1`。默认单价复算为：文本输入 × 5/百万 + 图片输入 × 8/百万 + 图片输出 × 30/百万。四次成功调用按真实 token 估算的上游费用合计 USD 0.032157；平台内部账单合计 USD 0.149177，差额来自显式按张配置。没有取得供应商实际扣费回执，不能把估算当实际账单。

4 条用量与4条资金去重记录，归属各自测试 Key/账号；总扣费 USD 0.149177，测试用户余额由 20 变为 19.850823，Key 总配额增量 0.149177。每笔用量、费用分项、计费模式、总额及余额/配额差额均通过脚本对账。

验收后已停用测试 Key、分组和账号副本，删除对应 Redis 鉴权缓存并发布本地缓存失效，停止本轮 API 进程；保留账单与私有证据，不删除审计记录。私有配置、脚本、图片和证据仅在 `/Users/suqing/Coding/golang/00_self/sub2api/deploy/test-baseline/too-674/` 及忽略的 `backend/data/too674-acceptance/` 下，不提交 Git。

本机 `/tmp` 日志不保证跨机器或清理后存在。真实环境只有 API Key 账号；OAuth 直连、流式和图片缓存明细由模拟回归覆盖，没有宣称完成真实 OAuth/WS 生图验收。前端、全仓集成测试和 CI 未运行。真实调用使用的构建早于将流式字段合并简化为共享 helper 的等价整理，该最终整理由上述定向 race 覆盖；不能将真实调用推广为所有最终代码路径均实测。

## 上游关联与移除条件

- 官方参考：[图片生成及当前 token/缓存口径](https://developers.openai.com/api/docs/guides/image-generation)。官方明确 GPT Image 2/2.5 的缓存折扣仅适用于 Responses 图片工具；响应 usage 不能核实该工具的缓存命中。上述真实 Images 调用均未报告缓存；缓存分桶测试只验证明确上报的明细，不推断供应商隐藏缓存。
- 上游缺陷点：`Wei-Shaw/sub2api@d851a3847` 的 `backend/internal/service/openai_gateway_usage.go` 图片分支及 API Key Images 用量解析；未创建上游 Issue/PR。
- 上游正式版提供等价的默认 token 选择与图片用量归类后，必须跑相同定价优先级及入账回归，再移除此补丁；不能仅凭版本号或相似提交标题去重。

## 对抗式审查

核对了显式价格优先级、零价、缺失数据的旧契约、缓存互斥、图片输出不重复按文本收费、默认 token 模式的倍率持久化，以及流式合并是否丢失缓存写入。已修正实际发现的模式标记和 API Key 图片用量归类问题；没有引入重试、估算用量、历史账单修正或新数据库字段。剩余交付边界为源码提交/推送、人工评审、stable 集成和部署，分别按后续授权处理。
