# 阿里百炼 TTS

对应 [TOO-680](https://linear.app/sisyphus-sq/issue/TOO-680)。本功能提供 API Key 平台 `bailian`，复用网关分组、权限、模型映射、账号调度、并发和余额限制。音频默认转发或临时预览，不归档到服务器永久文件库。

## 账号与模型

在账号管理中选择「阿里百炼 TTS」，填写百炼 API Key。`base_url` 使用服务根地址，例如 `https://dashscope.aliyuncs.com`，新加坡使用 `https://dashscope-intl.aliyuncs.com`；也可使用对应业务空间的专属域名。不要填写 `/compatible-mode/v1` 或具体 API 路径。各地域 Key 和模型权限分别生效。

主要模型：

| 模型 | HTTP 非流式 | HTTP SSE | WebSocket | 定价单位 |
| --- | --- | --- | --- | --- |
| qwen-audio-3.1-tts-plus | 支持 | 支持 | inference 任务协议 | 百万计费字符 |
| qwen-audio-3.1-tts-flash | 支持 | 支持 | inference 任务协议 | 输入、输出 Token |
| qwen-audio-3.1-tts-next | 支持 | 不支持 | 不支持 | 输入、输出 Token |
| Qwen-Audio 3.0 TTS、CosyVoice | 支持 | 支持 | inference 任务协议 | 百万计费字符 |
| qwen3-tts-flash | 支持 | 支持 | 使用独立 realtime 模型 | 百万计费字符 |
| qwen3-tts-flash-realtime | 使用独立 HTTP 模型 | 使用独立 HTTP 模型 | realtime 会话协议 | 百万计费字符 |

内置目录是已适配的模型清单，不表示当前 Key 对每个模型都有权限。模型和音色必须匹配；CosyVoice 的音色不能当作 Qwen 音色使用。声音复刻/声音设计得到的音色 ID 可在对应模型中使用。

## 统一语音入口

`POST /v1/audio/speech`，使用 Sub2API 下游 Key 鉴权。以下 `stream` 和 `bailian_parameters` 是本项目扩展字段。

```json
{
  "model": "qwen-audio-3.1-tts-plus",
  "input": "你好，欢迎使用百炼语音。",
  "voice": "longanhuan_v3.1",
  "response_format": "mp3",
  "stream": true,
  "bailian_parameters": {"sample_rate": 24000}
}
```

- `stream=false`：等待上游合成完成，安全取回上游音频并返回音频字节。
- `stream=true`：解析上游 SSE 的 Base64 片段，按顺序输出音频字节并刷新。响应声明 `X-Sub2API-Stream-Status` trailer，完整结束为 `complete`，上游中断为 `incomplete`。开始音频输出后无法再改写 HTTP 状态，调用方应检查流完成状态；需要结构化结束事件时使用原生 SSE。
- `response_format`、`speed`、`instructions` 分别映射原生格式、语速和指令。其他原生参数放在 `bailian_parameters`。不做音频转码，不把不支持的格式静默改成其他格式。
- `qwen3-tts-flash` 的 HTTP 输出为 WAV，不支持统一入口的语速设置。
- `next` 的 `input` 是场景提示词，映射成 `input.text_prompt`，不能传系统音色 ID。参考音频通过 `bailian_parameters.references` 传递。其流式请求直接返回 400，不把完成文件下载包装成生成流式。

## 百炼原生入口

客户端使用 Sub2API Key 代替百炼 Key，将服务根地址改为网关；网关从选定账号读取上游凭据。

- `POST /api/v1/services/audio/tts/SpeechSynthesizer`：Qwen-Audio-TTS、CosyVoice、next。
- `POST /api/v1/services/aigc/multimodal-generation/generation`：Qwen3-TTS。
- HTTP 非流式保留 JSON 中的音频 URL；该 URL 通常在 24 小时后失效。`X-DashScope-SSE: enable` 开启原生 SSE，保留音频、时间戳、用量及完成事件。
- `GET /api-ws/v1/inference`：首帧必须是 `run-task`，从 `payload.model` 选择账号。随后使用 `continue-task`、`finish-task` 等原生事件，音频以二进制帧转发。任务结束后可在同一连接内顺序启动同一模型的新任务。
- `GET /api-ws/v1/realtime?model=qwen3-tts-flash-realtime`：使用 `session.update`、`input_text_buffer.append/commit`、`session.finish`，保留 JSON 音频事件。
- 切换模型需要新连接；逐段审核与余额预留在文本发往上游之前执行。单个 WS 消息及连接累计审核文本限制为 1 MiB，inference 首帧等待上限 15 秒。

## 计费与故障语义

必须给实际计费模型配置显式分组/渠道价格。字符模型选 `per_request`，`per_request_price` 表示每百万计费字符单价；Token 模型选 `token`。后台价卡 API 的 `input_price/output_price` 按 **美元/Token** 保存，界面可显示每百万 Token 的价格，不可把百万 Token 价格直接写入 API。

按上游实际 `usage` 结算，不用 UTF-8 字节数或原始字符串长度冒充计费量。百炼汉字通常计为两个字符；`audio_input_characters` 与输入/输出 Token 分别持久化。SSE 累计用量不重复相加；WS 按任务/response 结算，客户端复用 `task_id` 不能绕过扣费。字符数明确为零时费用为零，不回落到一次调用价格。

配置缺失或单位不匹配在上游调用前拒绝。缺失/无效用量、缺失完成事件、音频下载失败和客户端取消均不触发重新合成；已确认的上游用量仍结算，未知剩余用量记录错误供排查，不伪造为免费或成功。同步结算在释放文本片段的余额预留之前完成。

sqmc03 本次部署采用用户指定固定比例 **1 USD = 7 CNY**，北京地域、不额外加价：

| 模型 | 官方人民币单价 | 网关美元价 |
| --- | --- | --- |
| plus | 1.4 元/万字符 | 20 美元/百万计费字符 |
| flash | 输入 1.5、输出 12 元/百万 Token | 输入 0.2142857142857143、输出 1.7142857142857142 美元/百万 Token |
| next | 输入 6、输出 12 元/百万 Token | 输入 0.8571428571428571、输出 1.7142857142857142 美元/百万 Token |

这些是固定换算规则，不随汇率自动变化。官方价格变动需显式更新价卡。

## 后台测试与验证

账号测试可选择非流式或 SSE，填写音色 ID。流式测试显示首段耗时并提供最大 4 MiB 的临时预览；`next` 使用非流式。WS 可使用原生 SDK 或显式启用的 `bailian_live` 测试，测试会产生真实费用；所需环境变量见测试源码，不在仓库保存 Key。

迁移 `249_bailian_audio_usage.sql` 仅增加 `audio_input_characters`，历史行默认 0；不会据历史 Token 反推字符数。部署前备份数据库及应用数据，回退旧二进制可保留该新增列。

详细证据和覆盖范围见 [TOO-680 验证记录](local/too-680-verification.md)。上游正式支持这些协议并通过相同流式、音频、异常和账本回归后，可评估移除本地补丁。

官方资料：[HTTP TTS](https://help.aliyun.com/zh/model-studio/qwen-audio-tts-http-api)、[next](https://help.aliyun.com/zh/model-studio/audio-generation-api)、[实时合成](https://help.aliyun.com/zh/model-studio/realtime-tts-user-guide)、[模型定价](https://help.aliyun.com/zh/model-studio/model-pricing)。
