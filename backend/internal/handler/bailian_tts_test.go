package handler

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	ws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBailianSpeechClassifiesBodyReadFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/audio/speech", "/api/v1/services/audio/tts/SpeechSynthesizer", "/api/v1/services/aigc/multimodal-generation/generation"} {
		for _, tc := range []struct {
			name       string
			body       io.Reader
			limit      int64
			budget     int64
			cancelled  bool
			wantStatus int
			message    string
		}{
			{name: "too large", body: strings.NewReader("123456"), limit: 2, wantStatus: http.StatusRequestEntityTooLarge, message: "Request body too large"},
			{name: "timeout", body: iotest.ErrReader(&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}), wantStatus: http.StatusRequestTimeout, message: "Timed out reading request body"},
			{name: "truncated", body: iotest.ErrReader(io.ErrUnexpectedEOF), wantStatus: http.StatusBadRequest, message: "Failed to read request body"},
			{name: "memory budget", body: strings.NewReader(`{"model":"qwen-audio-3.1-tts-plus"}`), budget: 1, wantStatus: http.StatusServiceUnavailable, message: "Request body buffering capacity is exhausted"},
			{name: "cancelled", body: strings.NewReader(`{}`), cancelled: true, wantStatus: statusClientClosedRequest},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				router := gin.New()
				limit := tc.limit
				if limit == 0 {
					limit = 1 << 20
				}
				router.POST(path, middleware.RequestBodyLimit(limit, pkghttputil.NewBodyMemoryBudget(tc.budget)), (&OpenAIGatewayHandler{}).BailianSpeech)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.cancelled {
					cancel()
				}
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, path, tc.body).WithContext(ctx)
				router.ServeHTTP(recorder, request)
				require.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
				if tc.cancelled {
					require.Empty(t, recorder.Body.String())
				} else {
					require.Contains(t, recorder.Body.String(), tc.message)
				}
				if tc.budget > 0 {
					require.Equal(t, "1", recorder.Header().Get("Retry-After"))
				}
			})
		}
	}
}

func TestBailianRealtimeRejectsDuplicateFieldsInFirstFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := &concurrencyCacheMock{acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) {
		return true, nil
	}}
	h := newOpenAIImageChatRejectionHandlerWithCache(t, cache)
	router := gin.New()
	router.GET("/api-ws/v1/inference", func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformBailian}})
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1, Concurrency: 1})
		h.BailianRealtime(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api-ws/v1/inference", nil)
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()
	err = client.Write(ctx, ws.MessageText, []byte(`{"header":{"action":"run-task"},"payload":{"model":"a","model":"b"}}`))
	require.NoError(t, err)
	_, body, err := client.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(body), "field more than once")
	_, _, err = client.Read(ctx)
	require.Equal(t, ws.StatusPolicyViolation, ws.CloseStatus(err))
}

func TestBailianSpeechRejectsBeforeAccountSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name, body, platform     string
		authenticated, allowlist bool
		status                   int
	}{
		{name: "invalid JSON", body: `{`, status: 400},
		{name: "array body", body: `[]`, status: 400},
		{name: "model missing", body: `{}`, status: 400},
		{name: "unauthenticated", body: `{"model":"qwen-audio-3.1-tts-plus"}`, status: 401},
		{name: "wrong group", body: `{"model":"qwen-audio-3.1-tts-plus"}`, platform: service.PlatformOpenAI, authenticated: true, status: 404},
		{name: "denied model", body: `{"model":"qwen-audio-3.1-tts-flash"}`, platform: service.PlatformBailian, authenticated: true, allowlist: true, status: 404},
		{name: "duplicate model uses last value", body: `{"model":"qwen-audio-3.1-tts-plus","model":"qwen-audio-3.1-tts-flash"}`, platform: service.PlatformBailian, authenticated: true, allowlist: true, status: 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(tt.body))
			if tt.authenticated {
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: tt.platform, ModelAllowlist: service.GroupModelAllowlist{Enabled: tt.allowlist, Models: []string{"qwen-audio-3.1-tts-plus"}}}})
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1, Concurrency: 1})
			}
			(&OpenAIGatewayHandler{}).BailianSpeech(c)
			require.Equal(t, tt.status, recorder.Code, recorder.Body.String())
		})
	}
}
