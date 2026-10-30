package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_decisions"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// resolveDecisionsPricing 在调用上游前固定端点定价，禁止回落到聊天模型的输出价或无价零成本。
func (s *OpenAIGatewayService) resolveDecisionsPricing(ctx context.Context, apiKey *APIKey, requestedModel, billingModel, upstreamModel string) (*ResolvedPricing, error) {
	if s.billingService == nil {
		return nil, errors.New("decisions billing service is unavailable")
	}
	for _, model := range usageBillingModelCandidates(billingModel, requestedModel, upstreamModel) {
		if pricing := s.resolveOpenAIChannelPricing(ctx, model, apiKey); pricing != nil {
			// 配置结果可能来自共享缓存；深拷贝后再交给异步记账。
			encoded, err := json.Marshal(pricing)
			if err != nil {
				return nil, fmt.Errorf("snapshot Decisions pricing: %w", err)
			}
			var snapshot ResolvedPricing
			if err := json.Unmarshal(encoded, &snapshot); err != nil {
				return nil, fmt.Errorf("snapshot Decisions pricing: %w", err)
			}
			snapshot.longContextPricingEnabled = pricing.longContextPricingEnabled
			if pricing.channelPricing != nil {
				encoded, err = json.Marshal(pricing.channelPricing)
				if err != nil {
					return nil, err
				}
				if err := json.Unmarshal(encoded, &snapshot.channelPricing); err != nil {
					return nil, err
				}
			}
			if snapshot.Mode == BillingModeToken && snapshot.BasePricing == nil && len(snapshot.Intervals) == 0 {
				return nil, ErrModelPricingUnavailable
			}
			return &snapshot, nil
		}
	}
	if upstreamModel != "gpt-6-luna" {
		return nil, fmt.Errorf("decisions model has no endpoint pricing: %w", ErrModelPricingUnavailable)
	}
	return &ResolvedPricing{
		Mode: BillingModeToken, Source: PricingSourceFallback,
		longContextPricingEnabled: apiKey == nil || apiKey.Group == nil || apiKey.Group.LongContextPricingEnabled,
		BasePricing: &ModelPricing{
			InputPricePerToken: 0.1e-6, CacheReadPricePerToken: 0.01e-6,
			CacheCreationPricePerToken: 0.125e-6, CacheCreationPriceExplicit: true,
			LongContextInputThreshold: 272000, LongContextInputMultiplier: 2,
		},
	}, nil
}

// ForwardDecisions 转发原生协议。仅明确拒绝执行的 429 允许切换账号；结果不确定时不重放。
func (s *OpenAIGatewayService) ForwardDecisions(ctx context.Context, c *gin.Context, account *Account, body []byte, mapping ChannelMappingResult, apiKey *APIKey) (*OpenAIForwardResult, error) {
	started := time.Now()
	if !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityDecisions) {
		writeOpenAIEmbeddingsError(c, http.StatusBadRequest, "invalid_request_error", "Account does not support native Decisions")
		return nil, errors.New("account does not support native Decisions")
	}
	req, err := openai_decisions.ParseRequest(body)
	if err != nil {
		writeOpenAIEmbeddingsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	forwardModel := req.Model
	if mapping.Mapped {
		forwardModel = mapping.MappedModel
	}
	billingModel := resolveOpenAIForwardModel(account, forwardModel, "")
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	pricingModel := billingModel
	switch mapping.BillingModelSource {
	case BillingModelSourceRequested:
		pricingModel = req.Model
	case BillingModelSourceChannelMapped:
		pricingModel = forwardModel
	case BillingModelSourceUpstream:
		pricingModel = upstreamModel
	}
	pricing, err := s.resolveDecisionsPricing(ctx, apiKey, req.Model, pricingModel, upstreamModel)
	if err != nil {
		writeOpenAIEmbeddingsError(c, http.StatusServiceUnavailable, "configuration_error", "Decisions pricing is unavailable")
		return nil, err
	}
	upstreamBody := body
	if req.Model != upstreamModel {
		upstreamBody = ReplaceModelInBody(body, upstreamModel)
	}
	credentialAccount, err := resolveCredentialAccount(ctx, s.accountRepo, account)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(credentialAccount.GetOpenAIProtocolAPIKey())
	if key == "" {
		return nil, errors.New("decisions account is missing API key")
	}
	baseURL := credentialAccount.GetOpenAIFormatBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	validatedURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Decisions base URL: %w", err)
	}
	upstreamRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, buildOpenAIEndpointURL(validatedURL, openai_decisions.Endpoint), bytes.NewReader(upstreamBody))
	if err != nil {
		return nil, err
	}
	upstreamRequest = upstreamRequest.WithContext(WithHTTPUpstreamProfile(upstreamRequest.Context(), HTTPUpstreamProfileOpenAI))
	upstreamRequest.Header.Set("Content-Type", "application/json")
	upstreamRequest.Header.Set("Accept", "application/json")
	for name, values := range c.Request.Header {
		if openaiCCRawAllowedHeaders[strings.ToLower(name)] {
			for _, value := range values {
				upstreamRequest.Header.Add(name, value)
			}
		}
	}
	credentialAccount.ApplyHeaderOverrides(upstreamRequest.Header)
	// 凭据只从选中的原生账号获取，客户端请求头和账号通用覆写不能替代它。
	upstreamRequest.Header.Set("Authorization", "Bearer "+key)
	proxyURL := ""
	if credentialAccount.ProxyID != nil && credentialAccount.Proxy != nil {
		proxyURL = credentialAccount.Proxy.URL()
	}
	SetOpsUpstreamModel(c, upstreamModel)
	response, err := s.doOpenAIUpstream(upstreamRequest, proxyURL, account)
	if err != nil {
		status, message := http.StatusBadGateway, "Upstream result is unknown; request was not replayed"
		var networkError net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
			status = http.StatusGatewayTimeout
		}
		s.recordDecisionsFailure(c, account, 0, "", "result_unknown", message)
		if !errors.Is(ctx.Err(), context.Canceled) {
			writeOpenAIEmbeddingsError(c, status, "upstream_error", message)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("decisions upstream result is unknown")
	}
	defer func() { _ = response.Body.Close() }()
	requestID := firstNonEmptyString(response.Header.Get("x-request-id"), response.Header.Get("request-id"))
	responseBody, err := ReadUpstreamResponseBody(response.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		s.recordDecisionsFailure(c, account, response.StatusCode, requestID, "result_unknown", "Decisions response could not be read; request was not replayed")
		if !c.Writer.Written() && !errors.Is(ctx.Err(), context.Canceled) {
			status := http.StatusBadGateway
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			writeOpenAIEmbeddingsError(c, status, "upstream_error", "Failed to read Decisions response")
		}
		return nil, fmt.Errorf("read Decisions response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// 保留错误对象和状态，仅清除可能被上游回显的凭据。
		responseBody = []byte(sanitizeUpstreamErrorMessage(string(responseBody)))
		responseBody = bytes.ReplaceAll(responseBody, []byte(key), []byte("[redacted]"))
		s.recordDecisionsFailure(c, account, response.StatusCode, requestID, "upstream_error", "Decisions upstream rejected the request")
		if response.StatusCode == http.StatusTooManyRequests {
			return nil, &UpstreamFailoverError{StatusCode: response.StatusCode, ResponseBody: responseBody, ResponseHeaders: response.Header.Clone()}
		}
		writeOpenAIEmbeddingsUpstreamResponse(c, response, responseBody, s.responseHeaderFilter)
		return nil, fmt.Errorf("decisions upstream returned status %d", response.StatusCode)
	}
	usage, err := openai_decisions.DecodeResponse(req, responseBody)
	if err != nil {
		s.recordDecisionsFailure(c, account, response.StatusCode, requestID, "invalid_response", "Decisions success response has invalid answers or usage; request was not replayed")
		writeOpenAIEmbeddingsError(c, http.StatusBadGateway, "upstream_error", "Invalid Decisions response or usage; request was not replayed")
		return nil, err
	}
	result := &OpenAIForwardResult{
		RequestID: firstNonEmptyString(requestID, "local:"+generateRequestID()), UpstreamHeaders: response.Header.Clone(),
		Model: req.Model, BillingModel: billingModel, UpstreamModel: upstreamModel,
		UpstreamResponseModel: gjson.GetBytes(responseBody, "model").String(), UpstreamEndpoint: openai_decisions.Endpoint,
		Duration: time.Since(started), DecisionsPricing: pricing,
		Usage: OpenAIUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CacheReadInputTokens: usage.CachedTokens, CacheCreationInputTokens: usage.CacheWriteTokens},
	}
	result.ClientDisconnect = ctx.Err() != nil
	if !result.ClientDisconnect {
		writeOpenAIEmbeddingsUpstreamResponse(c, response, responseBody, s.responseHeaderFilter)
	}
	return result, nil
}

func (s *OpenAIGatewayService) recordDecisionsFailure(c *gin.Context, account *Account, status int, requestID, kind, message string) {
	setOpsUpstreamError(c, status, message, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account), Platform: account.Platform,
		AccountID: account.ID, AccountName: account.Name, UpstreamStatusCode: status, UpstreamRequestID: requestID, Kind: kind, Message: message})
}
