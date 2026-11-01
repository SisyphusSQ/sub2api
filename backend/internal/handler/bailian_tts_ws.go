package handler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bailiantts"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	ws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// BailianRealtime 提供百炼原生双向流式入口。
func (h *OpenAIGatewayHandler) BailianRealtime(c *gin.Context) {
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || apiKey.Group == nil {
		h.errorResponse(c, 401, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, 500, "api_error", "User context not found")
		return
	}
	log := requestLogger(c, "handler.bailian_realtime")
	if !h.ensureResponsesDependencies(c, log) {
		return
	}
	started := false
	userRelease, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &started, log)
	if !acquired {
		return
	}
	if userRelease != nil {
		defer userRelease()
	}
	c.Set("bailian_ws_user_acquired", true)
	client, err := ws.Accept(c.Writer, c.Request, &ws.AcceptOptions{CompressionMode: ws.CompressionDisabled})
	if err != nil {
		return
	}
	defer func() { _ = client.CloseNow() }()
	client.SetReadLimit(1 << 20)
	fail := func(status int, kind, message string) {
		data, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message, "status": status}})
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), time.Second)
		defer cancel()
		_ = client.Write(writeCtx, ws.MessageText, data)
		_ = client.Close(ws.StatusPolicyViolation, "speech request rejected")
	}
	model := strings.TrimSpace(c.Query("model"))
	var first []byte
	if c.Request.URL.Path == bailiantts.InferencePath {
		readCtx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		kind, body, readErr := client.Read(readCtx)
		cancel()
		if readErr != nil || kind != ws.MessageText || gjson.GetBytes(body, "header.action").String() != "run-task" {
			fail(400, "invalid_request_error", "First event must be run-task")
			return
		}
		var event map[string]any
		if err := json.Unmarshal(body, &event); err != nil || event == nil {
			fail(400, "invalid_request_error", "First event must be a JSON object")
			return
		}
		body, _ = json.Marshal(event)
		if gjson.GetBytes(body, "header.action").String() != "run-task" {
			fail(400, "invalid_request_error", "First event must be run-task")
			return
		}
		first = body
		model = gjson.GetBytes(body, "payload.model").String()
	}
	if model == "" {
		fail(400, "invalid_request_error", "model is required")
		return
	}
	body, _ := json.Marshal(map[string]string{"model": model})
	h.withBailianAccount(c, model, body, fail, func(account *service.Account, mapping service.ChannelMappingResult, record func(*service.OpenAIForwardResult) error) error {
		var releases []func()
		defer func() {
			for _, release := range releases {
				release()
			}
		}()
		var textHistory strings.Builder
		authorize := func(text string) error {
			if textHistory.Len()+len(text) > 1<<20 {
				return errors.New("speech text event exceeds limit")
			}
			_, _ = textHistory.WriteString(text)
			audit, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": textHistory.String()}}})
			if decision := h.checkSecurityAudit(c, log, apiKey, subject, service.ContentModerationProtocolOpenAIChat, model, audit); decision != nil && !decision.AllowNextStage {
				return errors.New("speech content was rejected")
			}
			subscription, _ := middleware.GetSubscriptionFromContext(c)
			estimate := service.InflightEstimateRequest{Model: model, Kind: service.InflightEstimateToken, BodyBytes: len(text), MaxTokens: len([]rune(text))*16 + 128}
			if strings.Contains(model, "tts-plus") || strings.HasPrefix(model, "cosyvoice-") || strings.HasPrefix(model, "qwen3-tts-") {
				estimate.Kind = service.InflightEstimateAudio
				estimate.AudioMode = "tts"
				estimate.AudioUnits = float64(2*len([]rune(text))) / 1e6
			}
			release, err := reserveInflightBalance(c, h.billingCacheService, h.gatewayService, apiKey, subscription, estimate)
			if err != nil {
				return err
			}
			releases = append(releases, release)
			return nil
		}
		return h.gatewayService.ProxyBailianTTS(c.Request.Context(), c, client, account, model, first, apiKey, mapping, authorize, record)
	})
}
