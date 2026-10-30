# OpenAI Decisions 接入

本地补丁对应 [TOO-675](https://linear.app/sisyphus-sq/issue/TOO-675)。契约核对日期：2026-10-30。

## 范围与配置

网关提供 `POST /v1/decisions`，使用 Sub2API 下游 API Key。分组必须路由到 OpenAI 平台；上游账号必须使用原生 OpenAI API Key。ChatGPT OAuth、PAT、Codex setup token、OpenRouter 转换和 System One 桥接不在本补丁范围。

复用已有分组模型白名单、渠道映射、账号模型映射、额度、并发和审计控制。分组白名单针对客户端请求的模型；渠道映射后再应用账号映射。被分组禁用的模型沿用现有网关语义返回 404，正文说明模型对该分组不可用。

账号没有 `credentials.openai_capabilities` 时，原生 API Key 账号默认具备 Decisions 候选资格。显式能力列表必须包含 `decisions`，例如：

```json
{"openai_capabilities":["chat_completions","embeddings","decisions"]}
```

账号创建、编辑及批量编辑表单提供 Decisions API 选项。原有显式 `["chat_completions","embeddings"]` 限制会保留，不因编辑或批量更新而隐式开启 Decisions。配置候选资格不证明上游已支持该端点；上游不支持时按其错误响应返回，不进行聊天协议降级。

Base URL 可以为 `https://api.openai.com` 或包含 `/v1` 的形式，端点按现有版本路径规则拼接。上游密钥只从选中账号读取，替换下游 Authorization。请求体使用现有全局大小限制，支持图片所需的请求容量。

## 请求与响应

以下命令仅为示例，执行真实调用前须明确预算和环境。变量通过本机私有配置提供，不将真实密钥写入文档、Git 或测试输出。

```sh
curl --fail-with-body "${SUB2API_BASE_URL}/v1/decisions" \
  -H "Authorization: Bearer ${SUB2API_API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"gpt-6-luna",
    "input":"The package arrived with a broken screen.",
    "questions":[{
      "type":"predicate",
      "name":"damaged",
      "instructions":"Does the customer report a damaged item?"
    }]
  }'
```

支持 `predicate`、`choice`、`score`，每次 1～400 个问题。原始 JSON 保留，仅按已配置的模型映射替换 `model`。字符串输入、消息/内容数组、`input_json`、共享及问题级图片、`safety_identifier` 和缓存参数按原生协议转发。当前缓存 TTL 仅支持 `30m`，不支持流式调用。

响应保留答案顺序、重复或省略的问题名称、布尔值与字符串 choice，以及正常 `refusal`。模型拒答属于协议结果，按上游真实用量结算，不伪装成传输失败。

Prompt Audit 和既有内容审核覆盖共享输入、JSON 证据、问题说明、选项和评分等级描述。图片传入既有图片审核通道，原始图片字节不作为文本快照保存；既有审核数量和长度限制继续适用。

## 用量与计价

成功响应须有实际 `usage.input_tokens`、`usage.output_tokens`，以及 `usage.input_tokens_details.cached_tokens` 和 `cache_write_tokens`。零值有效；缺失、null、负数或缓存计数超过总输入均属于上游协议异常。不得估算或把未知用量当成零用量。

三个输入桶互斥：

```text
普通输入 = input_tokens - cached_tokens - cache_write_tokens
费用 = 普通输入 × 普通输入价 + 缓存读取 × 缓存读取价 + 缓存写入 × 缓存写入价
```

默认 `gpt-6-luna` Decisions 价格（USD / 百万 token）：普通输入 0.10、缓存读取 0.01、30m 缓存写入 0.125、输出 0。默认价与聊天接口价卡分开；长上下文按现有配置和模型阈值处理。分组/渠道显式售价及倍率沿用现有优先级，区域部署或特殊商业价格通过显式价格配置表达，不从自定义中转域名推断。

访问上游前解析并冻结定价，异步记账使用同一快照和请求定价时刻。未知模型且没有显式价格时返回 503 配置错误，不发送上游请求，也不按零成本放行。

用量记录保留用户、下游 Key、分组、实际账号、请求/上游/响应模型、实际端点与请求标识；保留请求体摘要用于扣费冲突检测。扣费复用现有事务及 `(request_id, api_key_id)` 去重。记账任务在工作池无法接收时走已有同步兜底，不因队列溢出丢弃。

资金与 Key 配额沿用项目逐笔 8 位小数量化规则；用量成本列保留 10 位。余额对账使用逐笔量化金额的合计。

## 错误、取消与结果未知

- 上游明确错误保留状态及错误对象，凭据脱敏。只有明确拒绝执行的 429 进入既有有界账号切换；耗尽时保留 429 和 `Retry-After`。
- 400/401/403/404/422/5xx 等响应直接终止，不自动重放。传输错误、读取失败、上游成功但答案或用量损坏均不重放。
- 超时返回 504；连接取消沿用内部 499 分类。上游执行结果未知时不生成正常用量账单，运维错误记录标明结果未知或响应无效。
- 客户端断开前后若已经完整取得合法上游结果及真实用量，仍结算该次真实调用；客户端请求状态与上游已产生的用量分别记录。
- 扣费失败明确输出带请求、账号和分组关联信息的错误，不落零费用成功账单。排查或恢复记账须沿用原标识，不能再次请求上游。本补丁不新增自动补账服务，异常账目需按错误记录核对后单独处理。

## 开发验证与真实验收

开发测试使用模拟 HTTP 上游、账号/并发测试桩及现有 SQL mock；不使用运行数据库或真实密钥。开发验证记录见 [本地验证记录](local/too-675-verification.md)。

2026-10-30 已在另行交接的专用 PostgreSQL/Redis 测试副本中完成真实验收：两个独立 OpenAI 分组共 12 次原生调用，覆盖直连/网关语义、真实缓存写入和读取；实际账号、用量桶、费用、余额及唯一扣费记录均已核对。另通过隔离模拟上游验证错误、取消、超时和凭据脱敏，并验证真实 PostgreSQL 并发去重及指纹冲突。详细结果及未覆盖边界见 [验收记录](local/too-675-verification.md)。模型概率不要求跨两次调用完全相同。

补丁不含数据库迁移、历史账单修正、稳定分支合入或服务部署。提交/推送收尾复用已有测试输出，不重复测试。

## 来源与维护

- [官方接口契约](https://developers.openai.com/api/reference/resources/decisions/methods/create)
- [官方使用、缓存及计价说明](https://developers.openai.com/api/docs/guides/decisions)
- [官方模型价格](https://developers.openai.com/api/docs/pricing)
- [上游 PR #7922](https://github.com/Wei-Shaw/sub2api/pull/7922)：仅作审查参考，其 200 问题上限和字段白名单未直接采用，OpenRouter 改动未整包合入。

上游正式版提供等价原生支持并通过本卡转发、权限及记账回归后，再评估移除补丁。
