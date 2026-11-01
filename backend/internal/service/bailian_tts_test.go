package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/bailiantts"
	ws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBailianSSEStreamsBeforeCompletion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/audio/speech", nil)
	model, _ := bailiantts.Lookup("qwen-audio-3.1-tts-plus")
	req := &bailiantts.Request{Model: model, Format: "mp3"}
	chunk := fmt.Sprintf("data: {\"output\":{\"audio\":{\"data\":\"%s\"}},\"usage\":{\"characters\":2}}\n\n", base64.StdEncoding.EncodeToString([]byte("audio-one")))
	stream := chunk + `data: {"output":{"finish_reason":"stop"},"usage":{"characters":2}}` + "\n\n"
	s := &OpenAIGatewayService{cfg: &config.Config{}}
	var usage bailiantts.Usage
	var first *int
	err := s.relayBailianSSE(context.Background(), c, strings.NewReader(stream), req, true, &usage, time.Now(), &first)
	require.NoError(t, err)
	require.Equal(t, "audio-one", rec.Body.String())
	require.True(t, rec.Flushed)
	require.NotNil(t, first)
	require.Equal(t, 2, usage.Characters)
}

func TestBailianWebSocketPreservesBinaryAndBillsReusedClientIDsSeparately(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-upstream" {
			w.WriteHeader(401)
			return
		}
		conn, err := ws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if _, _, err = conn.Read(ctx); err != nil {
			return
		}
		for _, message := range []struct {
			kind ws.MessageType
			body string
		}{
			{ws.MessageText, `{"header":{"event":"task-started","task_id":"reused"},"payload":{}}`},
			{ws.MessageBinary, "\x00\x01audio\xff"},
			{ws.MessageText, `{"header":{"event":"task-finished","task_id":"reused"},"payload":{"usage":{"characters":4}}}`},
		} {
			if err := conn.Write(ctx, message.kind, []byte(message.body)); err != nil {
				return
			}
		}
		_, _, _ = conn.Read(ctx)
	}))
	defer upstream.Close()
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	s := &OpenAIGatewayService{cfg: cfg, billingService: NewBillingService(cfg, nil)}
	s.resolver = NewModelPricingResolver(nil, s.billingService)
	price := 20.0
	model := "qwen-audio-3.1-tts-plus"
	key := &APIKey{Group: &Group{ID: 9, ModelPricing: []ChannelModelPricing{{Models: []string{model}, BillingMode: BillingModePerRequest, PerRequestPrice: &price}}}}
	account := &Account{ID: 7, Platform: PlatformBailian, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": upstream.URL, "api_key": "fixture-upstream"}}
	recorded := make(chan *OpenAIForwardResult, 2)
	finished := make(chan error, 2)
	router := gin.New()
	router.GET(bailiantts.InferencePath, func(c *gin.Context) {
		client, err := ws.Accept(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		defer func() { _ = client.CloseNow() }()
		finished <- s.ProxyBailianTTS(ctx, c, client, account, model, []byte(`{"header":{"action":"run-task","task_id":"reused"},"payload":{"model":"qwen-audio-3.1-tts-plus","input":{}}}`), key, ChannelMappingResult{}, func(string) error { return nil }, func(result *OpenAIForwardResult) error { recorded <- result; return nil })
	})
	downstream := httptest.NewServer(router)
	defer downstream.Close()
	var ids []string
	for range 2 {
		client, _, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(downstream.URL, "http")+bailiantts.InferencePath, nil)
		require.NoError(t, err)
		for index := range 3 {
			kind, body, err := client.Read(ctx)
			require.NoError(t, err)
			if index == 1 {
				require.Equal(t, ws.MessageBinary, kind)
				require.Equal(t, "\x00\x01audio\xff", string(body))
			}
		}
		_ = client.Close(ws.StatusNormalClosure, "done")
		select {
		case result := <-recorded:
			require.Equal(t, 4, result.BailianUsage.Characters)
			ids = append(ids, result.RequestID)
		case <-ctx.Done():
			t.Fatal("missing settlement")
		}
		select {
		case err := <-finished:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("relay did not exit")
		}
	}
	require.NotEqual(t, ids[0], ids[1], "reused client task IDs must not suppress billing for a new synthesis")
}

func TestBailianSSERejectsIncompleteAndMissingUsage(t *testing.T) {
	for _, stream := range []string{`data: {"output":{"audio":{"data":"YQ=="}},"usage":{"characters":1}}` + "\n\n", `data: {"output":{"finish_reason":"stop"}}` + "\n\n", `data: {"code":"InvalidApiKey"}` + "\n\n"} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/audio/speech", nil)
		model, _ := bailiantts.Lookup("qwen-audio-3.1-tts-plus")
		var usage bailiantts.Usage
		var first *int
		err := (&OpenAIGatewayService{cfg: &config.Config{}}).relayBailianSSE(context.Background(), c, strings.NewReader(stream), &bailiantts.Request{Model: model, Format: "mp3"}, true, &usage, time.Now(), &first)
		require.Error(t, err)
	}
}

func TestBailianPricingNeverFallsBackToGrok(t *testing.T) {
	s := &OpenAIGatewayService{cfg: &config.Config{}, billingService: NewBillingService(&config.Config{}, nil)}
	_, err := s.resolveBailianPricing(context.Background(), &APIKey{}, "qwen-audio-3.1-tts-plus", true)
	require.Error(t, err)
	u := bailiantts.Usage{Characters: 100, Present: true}
	price := 20.0
	p := &ResolvedPricing{Mode: BillingModePerRequest, DefaultPerRequestPrice: price}
	r := bailianResult("qwen-audio-3.1-tts-plus", "qwen-audio-3.1-tts-plus", bailiantts.SpeechPath, false, time.Now(), p, u)
	cost, err := s.calculateOpenAIRecordUsageCost(context.Background(), r, &APIKey{}, []string{r.Model}, 2, 1, 1, 1, UsageTokens{}, "", nil, false, time.Now())
	require.NoError(t, err)
	require.InDelta(t, .004, cost.ActualCost, 1e-10)
	require.True(t, isForcedUsageBillingRequestID(r.RequestID))
	require.True(t, strings.HasPrefix(r.RequestID, "bailian_tts:"))
}
