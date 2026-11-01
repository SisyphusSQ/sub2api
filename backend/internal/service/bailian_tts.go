package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bailiantts"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// BailianTTSModels 是管理端和模型列表的默认语音模型目录。
var BailianTTSModels = []string{"qwen-audio-3.1-tts-plus", "qwen-audio-3.1-tts-flash", "qwen-audio-3.1-tts-next", "qwen-audio-3.0-tts-plus", "qwen-audio-3.0-tts-flash", "qwen3-tts-flash", "qwen3-tts-flash-realtime", "cosyvoice-v3.5-plus", "cosyvoice-v3.5-flash", "cosyvoice-v3-plus", "cosyvoice-v3-flash", "cosyvoice-v2"}

// BailianTTSURL 只拼接已知协议路径；base_url 应为百炼地域或业务空间的服务根地址。
func (s *OpenAIGatewayService) BailianTTSURL(account *Account, path string) (string, error) {
	switch path {
	case bailiantts.SpeechPath, bailiantts.QwenPath, bailiantts.InferencePath, bailiantts.RealtimePath:
	default:
		return "", errors.New("unsupported Bailian endpoint")
	}
	base := strings.TrimRight(strings.TrimSpace(account.GetCredential("base_url")), "/")
	if base == "" {
		base = "https://dashscope.aliyuncs.com"
	}
	validated, err := s.validateUpstreamBaseURL(base)
	if err != nil {
		return "", errors.New("invalid Bailian base URL")
	}
	return strings.TrimRight(validated, "/") + path, nil
}

func (s *OpenAIGatewayService) resolveBailianPricing(ctx context.Context, apiKey *APIKey, model string, chars bool) (*ResolvedPricing, error) {
	p := s.resolveOpenAIChannelPricing(ctx, model, apiKey)
	if p == nil {
		return nil, fmt.Errorf("configure explicit Bailian model pricing: %w", ErrModelPricingUnavailable)
	}
	if (chars && p.Mode != BillingModePerRequest) || (!chars && p.Mode != BillingModeToken) {
		return nil, errors.New("bailian pricing unit does not match model: characters require per_request per million characters; flash/next require token pricing")
	}
	// 保存独立快照，请求或连接内的结算不受后续价卡替换影响。
	encoded, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var snapshot ResolvedPricing
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func bailianResult(model, upstream, endpoint string, stream bool, started time.Time, pricing *ResolvedPricing, usage bailiantts.Usage) *OpenAIForwardResult {
	id := usage.RequestID
	if id == "" {
		id = generateRequestID()
	}
	r := &OpenAIForwardResult{RequestID: "bailian_tts:" + id, Model: model, BillingModel: upstream, UpstreamModel: upstream, UpstreamEndpoint: endpoint, Stream: stream, OpenAIWSMode: endpoint == bailiantts.InferencePath || endpoint == bailiantts.RealtimePath, Duration: time.Since(started), BailianPricing: pricing, BailianUsage: &usage, Usage: OpenAIUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}}
	if info, ok := bailiantts.Lookup(upstream); ok && info.Characters {
		r.AudioUsage = &AudioUsage{Mode: "tts", DurationOrUnits: float64(usage.Characters) / 1e6}
	}
	return r
}

// ForwardBailianTTS 转发一次合成。已受理或结果未知的请求不会重放。
func (s *OpenAIGatewayService) ForwardBailianTTS(ctx context.Context, c *gin.Context, account *Account, body []byte, compatible bool, nativePath string, mapping ChannelMappingResult, apiKey *APIKey) (*OpenAIForwardResult, error) {
	started := time.Now()
	requested := gjson.GetBytes(body, "model").String()
	forward := requested
	if mapping.Mapped {
		forward = mapping.MappedModel
	}
	upstreamModel := resolveOpenAIForwardModel(account, forward, "")
	req, err := bailiantts.Prepare(body, compatible, upstreamModel, nativePath, c.GetHeader("X-DashScope-SSE") == "enable")
	if err != nil {
		writeOpenAIEmbeddingsError(c, 400, "invalid_request_error", err.Error())
		return nil, err
	}
	pricingModel := upstreamModel
	switch mapping.BillingModelSource {
	case BillingModelSourceRequested:
		pricingModel = requested
	case BillingModelSourceChannelMapped:
		pricingModel = forward
	}
	pricing, err := s.resolveBailianPricing(ctx, apiKey, pricingModel, req.Model.Characters)
	if err != nil {
		writeOpenAIEmbeddingsError(c, 503, "configuration_error", err.Error())
		return nil, err
	}
	credential, err := resolveCredentialAccount(ctx, s.accountRepo, account)
	if err != nil {
		return nil, err
	}
	target, err := s.BailianTTSURL(credential, req.Path)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(credential.GetCredential("api_key"))
	if key == "" {
		return nil, errors.New("bailian account is missing API key")
	}
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "application/json")
	if req.Stream {
		upReq.Header.Set("X-DashScope-SSE", "enable")
		upReq.Header.Set("Accept", "text/event-stream")
	}
	if workspace := credential.GetCredential("workspace_id"); workspace != "" {
		upReq.Header.Set("X-DashScope-WorkSpace", workspace)
	}
	upReq.Header.Set("Authorization", "Bearer "+key)
	proxy := ""
	if credential.Proxy != nil {
		proxy = credential.Proxy.URL()
	}
	SetOpsUpstreamModel(c, upstreamModel)
	resp, err := s.httpUpstream.Do(upReq, proxy, account.ID, account.Concurrency)
	if err != nil {
		return nil, errors.New("bailian request result is unknown; request was not replayed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, readErr := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
		if readErr != nil {
			return nil, readErr
		}
		data = bytes.ReplaceAll(data, []byte(key), []byte("[redacted]"))
		data = []byte(sanitizeUpstreamErrorMessage(string(data)))
		c.Data(resp.StatusCode, "application/json", data)
		return nil, fmt.Errorf("bailian upstream returned %d", resp.StatusCode)
	}
	usage := bailiantts.Usage{RequestID: resp.Header.Get("X-Request-ID")}
	if req.Stream {
		c.Header("Trailer", "X-Sub2API-Stream-Status")
		var first *int
		err = s.relayBailianSSE(ctx, c, resp.Body, req, compatible, &usage, started, &first)
		result := bailianResult(requested, upstreamModel, req.Path, true, started, pricing, usage)
		result.BillingModel = pricingModel
		if err != nil {
			c.Header("X-Sub2API-Stream-Status", "incomplete")
		} else {
			c.Header("X-Sub2API-Stream-Status", "complete")
		}
		result.FirstTokenMs = first
		result.ClientDisconnect = ctx.Err() != nil
		if err != nil {
			s.recordDecisionsFailure(c, account, 502, usage.RequestID, "bailian_stream_incomplete", "Bailian audio stream ended without confirmed completion; not replayed")
		}
		return result, err
	}
	data, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}
	usage.Observe(data, req.Model)
	result := bailianResult(requested, upstreamModel, req.Path, false, started, pricing, usage)
	result.BillingModel = pricingModel
	if !usage.Present || usage.Invalid || gjson.GetBytes(data, "output.finish_reason").String() != "stop" {
		return result, errors.New("bailian response is missing final usage or completion")
	}
	if gjson.GetBytes(data, "output.audio.url").String() == "" && gjson.GetBytes(data, "output.audio.data").String() == "" {
		return result, errors.New("bailian response has no audio")
	}
	if !compatible {
		c.Data(resp.StatusCode, "application/json", data)
		return result, nil
	}
	result.ClientDisconnect = ctx.Err() != nil
	if result.ClientDisconnect {
		return result, ctx.Err()
	}
	if encoded := gjson.GetBytes(data, "output.audio.data").String(); encoded != "" {
		audio, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr != nil {
			return result, errors.New("invalid Bailian audio encoding")
		}
		c.Data(200, bailiantts.MIME(req.Format), audio)
		return result, nil
	}
	url := gjson.GetBytes(data, "output.audio.url").String()
	if url == "" {
		return result, errors.New("bailian response has no audio")
	}
	// 上游返回的 URL 不可信：复用公网目的地、逐跳重定向和 DNS rebinding 防护。
	download, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return result, errors.New("invalid Bailian audio URL")
	}
	audio, err := defaultImageDownloadHTTPClient().Do(download)
	if err != nil {
		return result, errors.New("bailian audio download failed after generation; not replayed")
	}
	defer func() { _ = audio.Body.Close() }()
	if audio.StatusCode != 200 {
		return result, errors.New("bailian audio download rejected after generation")
	}
	limit := resolveUpstreamResponseReadLimit(s.cfg)
	if audio.ContentLength > limit {
		return result, errors.New("bailian audio exceeds response limit")
	}
	c.Header("Content-Type", bailiantts.MIME(req.Format))
	var size int64
	size, err = io.Copy(c.Writer, io.LimitReader(audio.Body, limit+1))
	result.ClientDisconnect = ctx.Err() != nil
	result.Duration = time.Since(started)
	if size > limit {
		return result, errors.New("bailian audio exceeds response limit")
	}
	return result, err
}

func (s *OpenAIGatewayService) relayBailianSSE(ctx context.Context, c *gin.Context, reader io.Reader, req *bailiantts.Request, compatible bool, usage *bailiantts.Usage, started time.Time, first **int) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), int(resolveUpstreamResponseReadLimit(s.cfg)))
	var event bytes.Buffer
	complete := false
	process := func() error {
		if event.Len() == 0 {
			return nil
		}
		raw := append([]byte(nil), event.Bytes()...)
		event.Reset()
		var lines []string
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "data:") {
				lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		if len(lines) == 0 {
			return nil
		}
		payload := []byte(strings.Join(lines, "\n"))
		if !gjson.ValidBytes(payload) {
			return errors.New("invalid Bailian SSE event")
		}
		usage.Observe(payload, req.Model)
		if usage.Invalid {
			return errors.New("bailian SSE returned invalid usage")
		}
		if code := gjson.GetBytes(payload, "code").String(); code != "" {
			return errors.New("bailian SSE returned an error")
		}
		terminal := gjson.GetBytes(payload, "output.finish_reason").String() == "stop"
		if terminal && !usage.Present {
			return errors.New("bailian SSE is missing final usage")
		}
		if compatible {
			if encoded := gjson.GetBytes(payload, "output.audio.data").String(); encoded != "" {
				audio, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil {
					return errors.New("invalid Bailian SSE audio")
				}
				if *first == nil {
					ms := int(time.Since(started).Milliseconds())
					*first = &ms
				}
				c.Header("Content-Type", bailiantts.MIME(req.Format))
				if _, err = c.Writer.Write(audio); err != nil {
					return err
				}
				c.Writer.Flush()
			}
		} else {
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("X-Accel-Buffering", "no")
			if *first == nil && gjson.GetBytes(payload, "output.audio.data").String() != "" {
				ms := int(time.Since(started).Milliseconds())
				*first = &ms
			}
			if _, err := c.Writer.Write(append(raw, '\n')); err != nil {
				return err
			}
			c.Writer.Flush()
		}
		complete = complete || terminal
		return ctx.Err()
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := process(); err != nil {
				return err
			}
		} else {
			_, _ = event.WriteString(line)
			_ = event.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return errors.New("bailian SSE read failed")
	}
	if err := process(); err != nil {
		return err
	}
	if !complete {
		return errors.New("bailian SSE ended without completion")
	}
	if *first == nil {
		return errors.New("bailian SSE completed without audio")
	}
	return nil
}
