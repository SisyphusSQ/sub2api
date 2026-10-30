package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func imageTokenBillingServiceForTest(t *testing.T) *BillingService {
	t.Helper()
	body, err := os.ReadFile("../../resources/model-pricing/model_prices_and_context_window.json")
	require.NoError(t, err)
	var catalog map[string]*LiteLLMModelPricing
	require.NoError(t, json.Unmarshal(body, &catalog))
	return NewBillingService(&config.Config{}, &PricingService{pricingData: catalog})
}

func TestOpenAIImageTokenBilling_NativeForwardUsage(t *testing.T) {
	for _, transport := range []string{"json", "stream_json", "sse"} {
		t.Run(transport, func(t *testing.T) {
			stream := transport != "json"
			body := []byte(fmt.Sprintf(`{"model":"gpt-image-2.5-flare","prompt":"red circle","stream":%t}`, stream))
			c, _ := newOpenAIImagesTestContext(t, body)
			payload := `{"usage":{"input_tokens":100,"input_tokens_details":{"image_tokens":80,"cached_tokens":50,"cached_tokens_details":{"image_tokens":40,"text_tokens":10}},"output_tokens":200},"data":[{"b64_json":"aGVsbG8="}]}`
			contentType := "application/json"
			if transport == "sse" {
				payload = "data: " + payload + "\n\ndata: [DONE]\n\n"
				contentType = "text/event-stream"
			}
			svc := newOpenAIImagesTestService(&httpUpstreamRecorder{resp: &http.Response{
				StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(payload)),
			}})
			parsed, err := svc.ParseOpenAIImagesRequest(c, body)
			require.NoError(t, err)
			result, err := svc.ForwardImages(context.Background(), c, &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "test-key", "base_url": "https://images.example/v1"}}, body, parsed, "")
			require.NoError(t, err)
			require.Equal(t, 1, result.ImageCount)
			require.Equal(t, 100, result.Usage.InputTokens)
			require.Equal(t, 80, result.Usage.ImageInputTokens)
			require.Equal(t, 50, result.Usage.CacheReadInputTokens)
			require.Equal(t, 40, result.Usage.ImageCacheReadTokens)
			require.Equal(t, 200, result.Usage.OutputTokens)
			require.Equal(t, 200, result.Usage.ImageOutputTokens)
		})
	}
}

func TestOpenAIImageTokenBilling_DefaultCatalog(t *testing.T) {
	for _, model := range []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst", "gpt-image-2.5-flare-2026-09-08"} {
		for _, withResolver := range []bool{false, true} {
			t.Run(model+"/resolver="+map[bool]string{false: "off", true: "on"}[withResolver], func(t *testing.T) {
				billing := imageTokenBillingServiceForTest(t)
				svc := &OpenAIGatewayService{billingService: billing}
				if withResolver {
					svc.resolver = NewModelPricingResolver(nil, billing)
				}
				groupID := int64(674)
				key := &APIKey{ID: 1, GroupID: &groupID, Group: &Group{ID: groupID, ImageRateMultiplier: 1}}
				// 普通输入50=文本10+图片40，缓存50=文本10+图片40；输出200全为图片。
				cost, err := svc.calculateOpenAIRecordUsageCost(context.Background(), &OpenAIForwardResult{
					Model: model, ImageCount: 1, ImageSize: "1K", ImageEndpoint: true,
				}, key, []string{model}, 2, 7, 1, 1, UsageTokens{
					InputTokens: 50, ImageInputTokens: 40, CacheReadTokens: 50, ImageCacheReadTokens: 40,
					OutputTokens: 200, ImageOutputTokens: 200,
				}, "", nil, false, time.Time{})
				require.NoError(t, err)
				require.Equal(t, string(BillingModeToken), cost.BillingMode)
				require.InDelta(t, 10*5e-6, cost.InputCost, 1e-12)
				require.InDelta(t, 40*8e-6, cost.ImageInputCost, 1e-12)
				require.InDelta(t, 10*1.25e-6+40*2e-6, cost.CacheReadCost, 1e-12)
				require.Zero(t, cost.OutputCost)
				require.InDelta(t, 200*30e-6, cost.ImageOutputCost, 1e-12)
				require.InDelta(t, 0.0064625, cost.TotalCost, 1e-12)
				require.InDelta(t, 0.0064625*2, cost.ActualCost, 1e-12)
			})
		}
	}
}

func TestOpenAIImageTokenBilling_Priority(t *testing.T) {
	const model = "gpt-image-2.5-flare"
	for _, tc := range []struct {
		name  string
		setup func(*OpenAIGatewayService, *APIKey, *OpenAIForwardResult, *UsageTokens) []string
		mode  BillingMode
		total float64
	}{
		{"default", nil, BillingModeToken, 0.0065},
		{"without_group", func(_ *OpenAIGatewayService, k *APIKey, _ *OpenAIForwardResult, _ *UsageTokens) []string {
			k.Group, k.GroupID = nil, nil
			return nil
		}, BillingModeToken, 0.0065},
		{"mixed_ignores_count_switch", func(_ *OpenAIGatewayService, k *APIKey, r *OpenAIForwardResult, _ *UsageTokens) []string {
			k.Group.ImageCountBilling, r.ImageEndpoint = true, false
			return nil
		}, BillingModeToken, 0.0065},
		{"count_switch", func(_ *OpenAIGatewayService, k *APIKey, _ *OpenAIForwardResult, _ *UsageTokens) []string {
			k.Group.ImageCountBilling = true
			k.Group.ImagePrice1K = f64p(0.4)
			return nil
		}, BillingModeImage, 0.8},
		{"count_switch_over_token_card", func(_ *OpenAIGatewayService, k *APIKey, _ *OpenAIForwardResult, _ *UsageTokens) []string {
			k.Group.ImageCountBilling = true
			k.Group.ImagePrice1K = f64p(0.4)
			k.Group.ModelPricing = []ChannelModelPricing{{Models: []string{model}, BillingMode: BillingModeToken, InputPrice: f64p(9e-6)}}
			return nil
		}, BillingModeImage, 0.8},
		{"group_size_price", func(_ *OpenAIGatewayService, k *APIKey, _ *OpenAIForwardResult, _ *UsageTokens) []string {
			k.Group.ImagePrice1K = f64p(0.3)
			return nil
		}, BillingModeImage, 0.6},
		{"group_size_free", func(_ *OpenAIGatewayService, k *APIKey, _ *OpenAIForwardResult, _ *UsageTokens) []string {
			k.Group.ImagePrice1K = f64p(0)
			return nil
		}, BillingModeImage, 0},
		{"unmatched_group_size", func(_ *OpenAIGatewayService, k *APIKey, _ *OpenAIForwardResult, _ *UsageTokens) []string {
			k.Group.ImagePrice4K = f64p(0.3)
			return nil
		}, BillingModeToken, 0.0065},
		{"group_token_over_size", func(_ *OpenAIGatewayService, k *APIKey, _ *OpenAIForwardResult, _ *UsageTokens) []string {
			k.Group.ImagePrice1K = f64p(0.3)
			k.Group.ModelPricing = []ChannelModelPricing{{Models: []string{model}, BillingMode: BillingModeToken, InputPrice: f64p(9e-6), ImageOutputPrice: f64p(21e-6)}}
			return nil
		}, BillingModeToken, 0.0051},
		{"group_image", func(_ *OpenAIGatewayService, k *APIKey, _ *OpenAIForwardResult, _ *UsageTokens) []string {
			k.Group.ImagePrice1K = f64p(0.3)
			k.Group.ModelPricing = []ChannelModelPricing{{Models: []string{model}, BillingMode: BillingModeImage, PerRequestPrice: f64p(0.2)}}
			return nil
		}, BillingModeImage, 0.4},
		{"group_per_request_free", func(_ *OpenAIGatewayService, k *APIKey, _ *OpenAIForwardResult, _ *UsageTokens) []string {
			k.Group.ModelPricing = []ChannelModelPricing{{Models: []string{model}, BillingMode: BillingModePerRequest, PerRequestPrice: f64p(0)}}
			return nil
		}, BillingModePerRequest, 0},
		{"channel_token", func(s *OpenAIGatewayService, k *APIKey, _ *OpenAIForwardResult, _ *UsageTokens) []string {
			s.resolver = newOpenAITokenImageChannelPricingResolverForTest(t, k.Group.ID, model)
			s.resolver.billingService = s.billingService
			k.Group.ImagePrice1K = f64p(0.3)
			return nil
		}, BillingModeToken, 0.0033},
		{"channel_image", func(s *OpenAIGatewayService, k *APIKey, _ *OpenAIForwardResult, _ *UsageTokens) []string {
			s.resolver = newOpenAIImageChannelPricingResolverForTest(t, k.Group.ID, model, 0.12)
			return nil
		}, BillingModeImage, 0.24},
		{"group_size_over_channel_image", func(s *OpenAIGatewayService, k *APIKey, _ *OpenAIForwardResult, _ *UsageTokens) []string {
			s.resolver = newOpenAIImageChannelPricingResolverForTest(t, k.Group.ID, model, 0.12)
			k.Group.ImagePrice1K = f64p(0.3)
			return nil
		}, BillingModeImage, 0.6},
		{"missing_usage_keeps_fallback", func(_ *OpenAIGatewayService, _ *APIKey, _ *OpenAIForwardResult, u *UsageTokens) []string {
			*u = UsageTokens{}
			return nil
		}, BillingModeImage, 0.268},
		{"unknown_model_keeps_fallback", func(_ *OpenAIGatewayService, _ *APIKey, r *OpenAIForwardResult, _ *UsageTokens) []string {
			r.Model = "unknown-image-too674"
			return []string{r.Model}
		}, BillingModeImage, 0.268},
		{"candidate_fallback", func(_ *OpenAIGatewayService, _ *APIKey, _ *OpenAIForwardResult, _ *UsageTokens) []string {
			return []string{"unknown-image-too674", model}
		}, BillingModeToken, 0.0065},
	} {
		t.Run(tc.name, func(t *testing.T) {
			billing := imageTokenBillingServiceForTest(t)
			svc := &OpenAIGatewayService{billingService: billing, resolver: NewModelPricingResolver(nil, billing)}
			gid := int64(674)
			key := &APIKey{ID: 1, GroupID: &gid, Group: &Group{ID: gid, ImageRateMultiplier: 1}}
			result := &OpenAIForwardResult{Model: model, ImageCount: 2, ImageSize: "1K", ImageEndpoint: true}
			tokens := UsageTokens{InputTokens: 100, OutputTokens: 200, ImageOutputTokens: 200}
			models := []string{model}
			if tc.setup != nil {
				if candidates := tc.setup(svc, key, result, &tokens); candidates != nil {
					models = candidates
				}
			}
			cost, err := svc.calculateOpenAIRecordUsageCost(context.Background(), result, key, models,
				2, 3, 1, 1, tokens, "", nil, false, time.Time{})
			require.NoError(t, err)
			require.Equal(t, string(tc.mode), cost.BillingMode)
			require.InDelta(t, tc.total, cost.TotalCost, 1e-12)
			multiplier := 3.0
			if tc.mode == BillingModeToken {
				multiplier = 2
			}
			require.InDelta(t, tc.total*multiplier, cost.ActualCost, 1e-12)
		})
	}
}

func TestOpenAIImageTokenBilling_RecordUsage(t *testing.T) {
	for _, withResolver := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "resolver"}[withResolver], func(t *testing.T) {
			logs := &openAIRecordUsageLogRepoStub{inserted: true}
			billings := &openAIRecordUsageBillingRepoStub{}
			svc := newOpenAIRecordUsageServiceForTest(logs, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
			svc.billingService = imageTokenBillingServiceForTest(t)
			svc.usageBillingRepo = billings
			if withResolver {
				svc.resolver = NewModelPricingResolver(nil, svc.billingService)
			}
			gid := int64(674)
			err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
				Result: &OpenAIForwardResult{RequestID: "too674-billing", Model: "gpt-image-2.5-flare", ImageCount: 1, ImageSize: "1K", ImageEndpoint: true,
					Usage: OpenAIUsage{InputTokens: 100, ImageInputTokens: 80, CacheReadInputTokens: 50, ImageCacheReadTokens: 40, OutputTokens: 200, ImageOutputTokens: 200}},
				APIKey: &APIKey{ID: 1, GroupID: &gid, Group: &Group{ID: gid, RateMultiplier: 2, ImageRateIndependent: true, ImageRateMultiplier: 7}},
				User:   &User{ID: 2}, Account: &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
			})
			require.NoError(t, err)
			require.Equal(t, 1, billings.calls)
			require.Equal(t, 1, logs.calls)
			require.Equal(t, string(BillingModeToken), *logs.lastLog.BillingMode)
			require.Equal(t, 2.0, logs.lastLog.RateMultiplier)
			require.Equal(t, 50, logs.lastLog.InputTokens)
			require.Equal(t, 80, logs.lastLog.ImageInputTokens)
			require.Equal(t, 50, logs.lastLog.CacheReadTokens)
			require.Equal(t, 200, logs.lastLog.ImageOutputTokens)
			require.InDelta(t, 0.0064625, logs.lastLog.TotalCost, 1e-12)
			require.InDelta(t, 0.012925, logs.lastLog.ActualCost, 1e-12)
			require.InDelta(t, 0.012925, billings.lastCmd.BalanceCost, 1e-12)
		})
	}
}
