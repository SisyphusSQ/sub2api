package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestDecisionsRouteUsesGroupModelAllowlist(t *testing.T) {
	router := newGatewayRoutesTestRouterWithGroup(allowlistGroup(service.PlatformOpenAI, true, "allowed-model"))
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(`{"model":"gpt-6-luna","input":"x","questions":[{"type":"predicate","instructions":"check"}]}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Contains(t, recorder.Body.String(), "not available for this group")
	require.Equal(t, "/v1/decisions", handler.NormalizeInboundEndpoint("/v1/decisions"))
}
