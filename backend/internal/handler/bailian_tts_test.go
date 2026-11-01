package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

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
