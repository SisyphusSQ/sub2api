package handler

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bailiantts"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// BailianSpeech 提供统一 audio/speech 与百炼原生 HTTP 入口。
func (h *OpenAIGatewayHandler) BailianSpeech(c *gin.Context) {
	reqLog := requestLogger(c, "handler.bailian_tts")
	body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if handleRequestBodyReadError(c, reqLog, err) {
			return
		}
		status, message := requestBodyReadFailure(reqLog, c.Request, err)
		h.errorResponse(c, status, "invalid_request_error", message)
		return
	}
	if !gjson.ValidBytes(body) {
		h.errorResponse(c, 400, "invalid_request_error", "Invalid speech JSON")
		return
	}
	// 审核与上游映射使用同一份规范 JSON，避免重复 input 键被不同解析器绑定。
	var canonical map[string]any
	if err := json.Unmarshal(body, &canonical); err != nil || canonical == nil {
		h.errorResponse(c, 400, "invalid_request_error", "Speech request must be an object")
		return
	}
	body, err = json.Marshal(canonical)
	if err != nil {
		h.errorResponse(c, 400, "invalid_request_error", "Invalid speech request")
		return
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" {
		h.errorResponse(c, 400, "invalid_request_error", "model is required")
		return
	}
	compatible := strings.HasSuffix(c.Request.URL.Path, "/audio/speech")
	path := ""
	if !compatible {
		path = c.Request.URL.Path
	}
	h.withBailianAccount(c, model, body, nil, func(account *service.Account, mapping service.ChannelMappingResult, record func(*service.OpenAIForwardResult) error) error {
		result, err := h.gatewayService.ForwardBailianTTS(c.Request.Context(), c, account, body, compatible, path, mapping, middlewareAPIKey(c))
		if result != nil {
			if billingErr := record(result); err == nil {
				err = billingErr
			}
		}
		return err
	})
}

func middlewareAPIKey(c *gin.Context) *service.APIKey {
	key, _ := middleware.GetAPIKeyFromContext(c)
	return key
}

// withBailianAccount 统一语音入口的权限、审核、并发、余额和记账生命周期。
// fail 可用于已升级的 WS，此时只能写协议错误并关闭，不能再写 HTTP JSON。
func (h *OpenAIGatewayHandler) withBailianAccount(c *gin.Context, model string, body []byte, fail func(int, string, string), run func(*service.Account, service.ChannelMappingResult, func(*service.OpenAIForwardResult) error) error) {
	if fail == nil {
		fail = func(status int, kind, message string) {
			if !c.Writer.Written() {
				h.errorResponse(c, status, kind, message)
			}
		}
	}
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || apiKey.Group == nil {
		fail(401, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		fail(500, "api_error", "User context not found")
		return
	}
	if !apiKey.Group.ModelAllowlist.Allows(model) { // Allows 的开启状态由下方显式判断约束。
		if apiKey.Group.ModelAllowlistEnabled() {
			fail(404, "not_found_error", "Model is not available for this group")
			return
		}
	}
	ensureCompositeTargetPlatform(c, apiKey, model)
	if effectiveAPIKeyPlatform(c, apiKey) != service.PlatformBailian || !compositeTargetPlatformAllowed(c, apiKey, model, service.PlatformBailian) {
		fail(404, "not_found_error", "Bailian TTS is not available for this group")
		return
	}
	log := requestLogger(c, "handler.bailian_tts", zap.String("model", model))
	if !h.ensureResponsesDependencies(c, log) {
		return
	}
	setOpsRequestContext(c, model, c.GetBool("bailian_ws_user_acquired") || gjson.GetBytes(body, "stream").Bool() || c.GetHeader("X-DashScope-SSE") == "enable")
	text := bailiantts.AuditText(body)
	if strings.TrimSpace(text) != "" {
		audit, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": text}}})
		if decision := h.checkSecurityAudit(c, log, apiKey, subject, service.ContentModerationProtocolOpenAIChat, model, audit); decision != nil && !decision.AllowNextStage {
			fail(403, "permission_error", "Speech content was rejected")
			return
		}
	}
	mapping, restricted := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, model)
	if restricted {
		fail(400, "invalid_request_error", "Channel does not allow this model")
		return
	}
	subscription, _ := middleware.GetSubscriptionFromContext(c)
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		status, kind, message, retry := billingErrorDetails(err)
		if retry > 0 {
			c.Header("Retry-After", strconv.Itoa(retry))
		}
		fail(status, kind, message)
		return
	}
	started := false
	if !c.GetBool("bailian_ws_user_acquired") {
		userRelease, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &started, log)
		if !acquired {
			return
		}
		if userRelease != nil {
			defer userRelease()
		}
	}
	// token 价卡按最大输出预算预留；字符价卡用整段输入的百万字符数预留。
	estimate := service.InflightEstimateRequest{Model: model, Kind: service.InflightEstimateToken, BodyBytes: len(body), MaxTokens: 8192}
	if strings.Contains(model, "tts-plus") || strings.HasPrefix(model, "cosyvoice-") || strings.HasPrefix(model, "qwen3-tts-") {
		estimate.Kind = service.InflightEstimateAudio
		estimate.AudioMode = "tts"
		estimate.AudioUnits = float64(2*len([]rune(text))) / 1e6
	}
	inflightDone, err := reserveInflightBalance(c, h.billingCacheService, h.gatewayService, apiKey, subscription, estimate)
	if err != nil {
		status, kind, message, _ := billingErrorDetails(err)
		fail(status, kind, message)
		return
	}
	defer inflightDone()
	forward := openAIChannelForwardModel(mapping, model)
	selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapability(c.Request.Context(), apiKey.GroupID, "", "", forward, nil, service.OpenAIUpstreamTransportHTTPSSE, service.OpenAIEndpointCapabilityBailianTTS, false, false, false, service.PlatformBailian)
	if err != nil || selection == nil || selection.Account == nil {
		fail(503, "api_error", "No available Bailian TTS accounts")
		return
	}
	account := selection.Account
	setOpsSelectedAccount(c, account.ID, account.Platform)
	release, slot := h.acquireOpenAIAccountSlot(c, apiKey.GroupID, "", selection, false, &started, log, func(status int, kind, code, message string) { fail(status, kind, message) })
	if slot != openAISlotAcquireOK {
		return
	}
	if release != nil {
		defer release()
	}
	record := func(result *service.OpenAIForwardResult) error {
		if result == nil {
			return nil
		}
		if result.BailianUsage == nil || !result.BailianUsage.Present {
			log.Error("bailian_tts.usage_missing", zap.String("request_id", result.RequestID))
			return errors.New("bailian usage is missing; settlement requires attention")
		}
		// 同步结算使所有文本片段的预留在账本完成前保持有效；客户端断开不取消结算。
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 10*time.Second)
		defer cancel()
		err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
			Result: result, APIKey: apiKey, User: apiKey.User, Account: account, Subscription: subscription,
			InboundEndpoint: GetInboundEndpoint(c), UpstreamEndpoint: result.UpstreamEndpoint,
			UserAgent: c.GetHeader("User-Agent"), IPAddress: ip.GetClientIP(c),
			RequestPayloadHash: service.HashUsageRequestPayload(body), APIKeyService: h.apiKeyService,
			QuotaPlatform:      service.QuotaPlatform(c.Request.Context(), apiKey),
			ChannelUsageFields: clientRequestedUsageFields(c, mapping, model, result.UpstreamModel),
		})
		if err != nil {
			log.Error("bailian_tts.record_usage_failed", zap.Error(err), zap.String("request_id", result.RequestID))
		}
		return err
	}
	if err := run(account, mapping, record); err != nil {
		log.Warn("bailian_tts.forward_failed", zap.Error(err))
		if c.Request.Context().Err() == nil {
			fail(502, "upstream_error", err.Error())
		}
	}
}
