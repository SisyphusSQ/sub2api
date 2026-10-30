//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type decisionsHandlerUpstream struct {
	service.HTTPUpstream
	statuses []int
	body     string
	hits     []int64
	cancel   context.CancelFunc
}

type decisionsHandlerUsageRepo struct {
	service.UsageLogRepository
	logs []*service.UsageLog
}

func (r *decisionsHandlerUsageRepo) Create(_ context.Context, log *service.UsageLog) (bool, error) {
	r.logs = append(r.logs, log)
	return true, nil
}

func (u *decisionsHandlerUpstream) Do(request *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.hits = append(u.hits, accountID)
	status := u.statuses[len(u.hits)-1]
	if u.cancel != nil {
		u.cancel()
	}
	body := u.body
	if status == 429 {
		body = `{"error":{"type":"rate_limit_error","message":"limited"}}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"15"}, "X-Request-Id": {"dec-handler"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestDecisionsHandlerFailoverIsBoundedAndSuccessIsRecorded(t *testing.T) {
	const requestBody = `{"model":"gpt-6-luna","input":"x","questions":[{"type":"predicate","instructions":"check"}]}`
	const responseBody = `{"model":"gpt-6-luna","answers":[{"type":"predicate","name":null,"probability":0.8}],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":0,"total_tokens":10}}`
	for _, tc := range []struct {
		name                 string
		statuses             []int
		body                 string
		wantStatus, wantHits int
	}{
		{"success", []int{200}, responseBody, 200, 1},
		{"rejected_then_success", []int{429, 200}, responseBody, 200, 2},
		{"rejected_exhausted", []int{429, 429}, responseBody, 429, 2},
		{"malformed_success", []int{200}, `{"model":"gpt-6-luna","answers":[]}`, 502, 1},
		{"disconnected_after_upstream_success", []int{200}, responseBody, 499, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			groupID := int64(901)
			repo := &grokCredentialHandlerRepo{accounts: []service.Account{
				{ID: 801, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 1, Concurrency: 1, GroupIDs: []int64{groupID}, Credentials: map[string]any{"api_key": "first-secret"}},
				{ID: 802, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 2, Concurrency: 1, GroupIDs: []int64{groupID}, Credentials: map[string]any{"api_key": "second-secret"}},
			}}
			upstream := &decisionsHandlerUpstream{statuses: tc.statuses, body: tc.body}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.wantStatus == 499 {
				upstream.cancel = cancel
			}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Default.RateMultiplier = 1
			billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billingCache.Stop)
			cache := &concurrencyCacheMock{acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil }, acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil }}
			usageRepo := &decisionsHandlerUsageRepo{}
			gateway := service.NewOpenAIGatewayService(repo, usageRepo, nil, nil, nil, nil, nil, cfg, nil, service.NewConcurrencyService(cache), service.NewBillingService(cfg, nil), nil, billingCache, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
			h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(cache), billingCache, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
			key := &service.APIKey{ID: 902, GroupID: &groupID, User: &service.User{ID: 903, Status: service.StatusActive}, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, RateMultiplier: 1, Status: service.StatusActive}}
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set(string(middleware.ContextKeyAPIKey), key)
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 903, Concurrency: 1})
				c.Next()
			})
			router.POST("/v1/decisions", h.Decisions)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(requestBody)).WithContext(ctx))
			require.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			require.Len(t, upstream.hits, tc.wantHits)
			require.Equal(t, int32(1), atomic.LoadInt32(&cache.releaseUserCalled))
			require.Equal(t, int32(tc.wantHits), atomic.LoadInt32(&cache.releaseAccountCalled))
			if tc.wantStatus == 429 {
				require.Equal(t, "15", recorder.Header().Get("Retry-After"))
			}
			if tc.wantStatus == 200 || tc.wantStatus == 499 {
				if tc.wantStatus == 200 {
					require.JSONEq(t, responseBody, recorder.Body.String())
				}
				require.Len(t, usageRepo.logs, 1)
				require.Equal(t, upstream.hits[len(upstream.hits)-1], usageRepo.logs[0].AccountID)
				require.Equal(t, groupID, *usageRepo.logs[0].GroupID)
			} else {
				require.Empty(t, usageRepo.logs)
			}
		})
	}
}

func TestDecisionsHandlerRequiresDownstreamCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(`{}`))
	(&OpenAIGatewayHandler{}).Decisions(c)
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}
