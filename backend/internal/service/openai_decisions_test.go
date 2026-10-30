package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const decisionsRequestFixture = `{"model":"gpt-6-luna","input":"evidence","questions":[{"type":"predicate","instructions":"check"}],"prompt_cache_key":"key"}`
const decisionsResponseFixture = `{"model":"gpt-6-luna","answers":[{"type":"predicate","name":null,"probability":0.8}],"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":200,"cache_write_tokens":100},"output_tokens":0,"total_tokens":1000}}`

func TestForwardDecisionsNativeProtocolAndUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(decisionsRequestFixture))
	c.Request.Header.Set("Authorization", "Bearer downstream-secret")
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"X-Request-Id": {"dec-123"}}, Body: io.NopCloser(strings.NewReader(decisionsResponseFixture))}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, billingService: NewBillingService(&config.Config{}, nil)}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "upstream-secret", "base_url": "https://api.openai.com/v1"}}
	result, err := svc.ForwardDecisions(c.Request.Context(), c, account, []byte(decisionsRequestFixture), ChannelMappingResult{}, &APIKey{})
	require.NoError(t, err)
	require.JSONEq(t, decisionsResponseFixture, recorder.Body.String())
	require.Equal(t, "https://api.openai.com/v1/decisions", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer upstream-secret", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, []byte(decisionsRequestFixture), upstream.lastBody)
	require.Equal(t, "dec-123", result.RequestID)
	require.Equal(t, 1000, result.Usage.InputTokens)
	require.Equal(t, 200, result.Usage.CacheReadInputTokens)
	require.Equal(t, 100, result.Usage.CacheCreationInputTokens)
	require.NotNil(t, result.DecisionsPricing)

	// 700 regular + 200 cache-read + 100 cache-write, each bucket counted once.
	cost, err := svc.calculateOpenAIRecordUsageCost(context.Background(), result, &APIKey{}, []string{"gpt-6-luna"}, 2, 1, 1, 1,
		UsageTokens{InputTokens: 700, CacheReadTokens: 200, CacheCreationTokens: 100, OutputTokens: 123}, "", nil, false, time.Time{})
	require.NoError(t, err)
	require.InDelta(t, 0.0000845, cost.TotalCost, 1e-12)
	require.InDelta(t, 0.000169, cost.ActualCost, 1e-12)
	require.Zero(t, cost.OutputCost)
}

func TestForwardDecisionsRejectsUnknownPriceBeforeUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(strings.Replace(decisionsRequestFixture, "gpt-6-luna", "unknown-model", 1))
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/decisions", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, billingService: NewBillingService(&config.Config{}, nil)}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "secret"}}
	result, err := svc.ForwardDecisions(context.Background(), c, account, body, ChannelMappingResult{}, &APIKey{})
	require.Error(t, err)
	require.Nil(t, result)
	require.Nil(t, upstream.lastReq)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

func TestForwardDecisionsMalformedSuccessIsTerminal(t *testing.T) {
	for _, response := range []string{`not-json`, `{"answers":[],"model":"gpt-6-luna"}`} {
		t.Run(response, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(decisionsRequestFixture))
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(response))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, billingService: NewBillingService(&config.Config{}, nil)}
			result, err := svc.ForwardDecisions(context.Background(), c, &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "secret"}}, []byte(decisionsRequestFixture), ChannelMappingResult{}, &APIKey{})
			require.Nil(t, result)
			require.Error(t, err)
			var retry *UpstreamFailoverError
			require.NotErrorAs(t, err, &retry)
			require.Equal(t, http.StatusBadGateway, recorder.Code)
		})
	}
}

func TestForwardDecisionsPreservesErrorsAndRedactsCredentials(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422, 500} {
		gin.SetMode(gin.TestMode)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(decisionsRequestFixture))
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"upstream_error","code":"bad_key","message":"upstream-secret"}}`))}}
		svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, billingService: NewBillingService(&config.Config{}, nil)}
		result, err := svc.ForwardDecisions(context.Background(), c, &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "upstream-secret"}}, []byte(decisionsRequestFixture), ChannelMappingResult{}, &APIKey{})
		require.Error(t, err)
		require.Nil(t, result)
		require.Equal(t, status, recorder.Code)
		require.NotContains(t, recorder.Body.String(), "upstream-secret")
		require.Contains(t, recorder.Body.String(), `"code":"bad_key"`)
	}
}

func TestDecisionsAccountCapabilityExcludesNonNativeCredentials(t *testing.T) {
	for _, typ := range []string{AccountTypeAPIKey, AccountTypeOAuth, AccountTypeSetupToken} {
		account := &Account{Platform: PlatformOpenAI, Type: typ}
		require.Equal(t, typ == AccountTypeAPIKey, account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityDecisions))
	}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"openai_capabilities": []string{"chat_completions"}}}
	require.False(t, account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityDecisions))
	account.Credentials["openai_capabilities"] = []string{"decisions"}
	require.True(t, account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityDecisions))
	account.Platform = PlatformAnthropic
	require.False(t, account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityDecisions))
}

func TestDecisionsBulkCapabilitiesPreserveExplicitRestrictions(t *testing.T) {
	legacy, chat, err := normalizeBulkOpenAIEndpointCapabilities([]string{"chat_completions", "embeddings"})
	require.NoError(t, err)
	require.True(t, chat)
	require.Equal(t, []string{"chat_completions", "embeddings"}, legacy)
	all, chat, err := normalizeBulkOpenAIEndpointCapabilities([]string{"decisions", "chat_completions", "embeddings"})
	require.NoError(t, err)
	require.True(t, chat)
	require.Nil(t, all)
	only, chat, err := normalizeBulkOpenAIEndpointCapabilities([]string{"decisions"})
	require.NoError(t, err)
	require.False(t, chat)
	require.Equal(t, []string{"decisions"}, only)
}

func TestDecisionsPricingSnapshotPreservesExplicitGroupPrice(t *testing.T) {
	input, output := 2e-6, 3e-6
	group := &Group{ID: 1, Platform: PlatformOpenAI, ModelPricing: []ChannelModelPricing{{Models: []string{"gpt-6-luna"}, BillingMode: BillingModeToken, InputPrice: &input, OutputPrice: &output}}}
	key := &APIKey{Group: group, GroupID: &group.ID}
	billing := NewBillingService(&config.Config{}, nil)
	svc := &OpenAIGatewayService{billingService: billing, resolver: NewModelPricingResolver(nil, billing)}
	pricing, err := svc.resolveDecisionsPricing(context.Background(), key, "gpt-6-luna", "gpt-6-luna", "gpt-6-luna")
	require.NoError(t, err)
	input = 99
	result := &OpenAIForwardResult{UpstreamEndpoint: "/v1/decisions", DecisionsPricing: pricing}
	cost, err := svc.calculateOpenAIRecordUsageCost(context.Background(), result, key, []string{"gpt-6-luna"}, 2, 1, 1, 1, UsageTokens{InputTokens: 100, OutputTokens: 10}, "", nil, false, time.Time{})
	require.NoError(t, err)
	require.InDelta(t, 0.00023, cost.TotalCost, 1e-12)
	require.InDelta(t, 0.00046, cost.ActualCost, 1e-12)
}

func TestForwardDecisionsTimeoutCancellationAndRateLimit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		upstreamErr error
		status      int
		canceled    bool
	}{
		{name: "deadline", upstreamErr: context.DeadlineExceeded, status: 504},
		{name: "disconnect", upstreamErr: context.Canceled, canceled: true},
		{name: "rate_limit", status: 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(decisionsRequestFixture)).WithContext(ctx)
			upstream := &httpUpstreamRecorder{err: tc.upstreamErr, resp: &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"15"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"limited"}}`))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, billingService: NewBillingService(&config.Config{}, nil)}
			result, err := svc.ForwardDecisions(ctx, c, &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "secret"}}, []byte(decisionsRequestFixture), ChannelMappingResult{}, &APIKey{})
			require.Nil(t, result)
			require.Error(t, err)
			require.Len(t, upstream.requests, 1)
			if tc.status == 429 {
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.Equal(t, "15", failover.ResponseHeaders.Get("Retry-After"))
				require.False(t, c.Writer.Written())
			} else if tc.canceled {
				require.False(t, c.Writer.Written())
			} else {
				require.Equal(t, tc.status, recorder.Code)
			}
		})
	}
}

func TestRecordDecisionsUsageKeepsOwnershipAndRejectsMissingPricing(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	billingRepo := &openAIRecordUsageBillingRepoStub{}
	svc.usageBillingRepo = billingRepo
	groupID := int64(41)
	key := &APIKey{ID: 2, GroupID: &groupID, Group: &Group{ID: groupID, Platform: PlatformOpenAI, RateMultiplier: 2}}
	pricing, err := svc.resolveDecisionsPricing(context.Background(), key, "gpt-6-luna", "gpt-6-luna", "gpt-6-luna")
	require.NoError(t, err)
	result := &OpenAIForwardResult{RequestID: "dec-usage", Model: "gpt-6-luna", UpstreamModel: "gpt-6-luna", UpstreamEndpoint: "/v1/decisions", DecisionsPricing: pricing, Usage: OpenAIUsage{InputTokens: 1000, CacheReadInputTokens: 200, CacheCreationInputTokens: 100}}
	input := &OpenAIRecordUsageInput{APIKey: key, User: &User{ID: 1}, Account: &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, Result: result, InboundEndpoint: "/v1/decisions", UpstreamEndpoint: "/v1/decisions"}
	require.NoError(t, svc.RecordUsage(context.Background(), input))
	require.Equal(t, int64(1), usageRepo.lastLog.UserID)
	require.Equal(t, int64(2), usageRepo.lastLog.APIKeyID)
	require.Equal(t, int64(3), usageRepo.lastLog.AccountID)
	require.Equal(t, groupID, *usageRepo.lastLog.GroupID)
	require.Equal(t, 700, usageRepo.lastLog.InputTokens)
	require.InDelta(t, 0.000169, usageRepo.lastLog.ActualCost, 1e-12)
	require.Equal(t, "dec-usage", billingRepo.lastCmd.RequestID)
	result.DecisionsPricing = nil
	require.Error(t, svc.RecordUsage(context.Background(), input))
	require.Equal(t, 1, usageRepo.calls)
	require.Equal(t, 1, billingRepo.calls)
	result.DecisionsPricing = pricing
	billingRepo.err = errors.New("database unavailable")
	require.Error(t, svc.RecordUsage(context.Background(), input))
	require.Equal(t, 1, usageRepo.calls, "failed billing must not produce a zero-cost success row")
}

func TestDecisionsModerationIncludesQuestionEvidence(t *testing.T) {
	input := ExtractContentModerationInput(ContentModerationProtocolOpenAIDecisions, []byte(`{"input":[{"type":"input_json","data":{"risk":"structured evidence"}}],"questions":[{"instructions":"question evidence","choices":[{"value":true,"description":"choice evidence"}]}]}`))
	for _, evidence := range []string{"structured evidence", "question evidence", "choice evidence"} {
		require.Contains(t, input.Text, evidence)
	}
}
