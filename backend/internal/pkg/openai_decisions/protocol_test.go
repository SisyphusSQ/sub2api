package openai_decisions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseRequestPreservesCurrentContract(t *testing.T) {
	body := []byte(`{"model":"alias","input":[{"role":"user","content":[{"type":"input_json","data":{"text":"evidence"}},{"type":"input_image","image_url":"https://example.com/a.png"}]}],"questions":[{"type":"choice","instructions":[{"type":"input_text","text":"choose"}],"choices":[{"value":true},{"value":"true"}]}],"prompt_cache_key":"partition","prompt_cache_options":{"mode":"explicit","ttl":"30m"},"future_extension":true}`)
	req, err := ParseRequest(body)
	require.NoError(t, err)
	require.Equal(t, "alias", req.Model)
	require.Len(t, req.Questions, 1)
}

func TestParseRequestQuestionBoundary(t *testing.T) {
	for _, count := range []int{0, 1, 200, 400, 401} {
		questions := make([]json.RawMessage, count)
		for i := range questions {
			questions[i] = json.RawMessage(`{"type":"predicate","instructions":"check"}`)
		}
		body, err := json.Marshal(map[string]any{"model": "gpt-6-luna", "input": "evidence", "questions": questions})
		require.NoError(t, err)
		_, err = ParseRequest(body)
		if count == 0 || count > 400 {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	}
}

func TestParseRequestRejectsInvalidRoutingAndCacheFields(t *testing.T) {
	for _, body := range []string{
		`[]`,
		`{"model":1,"input":"x","questions":[{}]}`,
		`{"model":"m","questions":[{"type":"predicate","instructions":"x"}]}`,
		`{"model":"m","input":"x","questions":[{"type":"chat","instructions":"x"}]}`,
		`{"model":"m","input":"x","questions":[{"type":"predicate","instructions":"x"}],"prompt_cache_options":{"ttl":"12h"}}`,
		`{"model":"m","input":"x","questions":[{"type":"predicate","instructions":"x"}],"stream":true}`,
	} {
		_, err := ParseRequest([]byte(body))
		require.Error(t, err, body)
	}
}

func TestParseRequestRejectsDuplicateTopLevelFields(t *testing.T) {
	_, err := ParseRequest([]byte(`{"model":"allowed-model","model":"forbidden-model","input":"x","questions":[{"type":"predicate","instructions":"check"}]}`))
	require.Error(t, err, "middleware and upstream must not interpret different model fields")
}

func TestDecodeResponseRequiresActualConsistentUsage(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"gpt-6-luna","input":"x","questions":[{"type":"predicate","instructions":"x"}]}`))
	require.NoError(t, err)
	valid := `{"model":"gpt-6-luna","answers":[{"type":"predicate","name":null,"probability":0.8}],"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":200,"cache_write_tokens":100},"output_tokens":0,"total_tokens":1000}}`
	usage, err := DecodeResponse(req, []byte(valid))
	require.NoError(t, err)
	require.Equal(t, Usage{InputTokens: 1000, CachedTokens: 200, CacheWriteTokens: 100}, usage)
	for _, body := range []string{
		strings.Replace(valid, `"input_tokens":1000`, `"input_tokens":1`, 1),
		strings.Replace(valid, `"input_tokens":1000,`, ``, 1),
		strings.Replace(valid, `"input_tokens":1000`, `"input_tokens":null`, 1),
		strings.Replace(valid, `"cached_tokens":200`, `"cached_tokens":-1`, 1),
		strings.Replace(valid, `"cached_tokens":200`, `"cached_tokens":null`, 1),
		strings.Replace(valid, `"answers":[{"type":"predicate","name":null,"probability":0.8}]`, `"answers":[]`, 1),
		strings.Replace(valid, `"probability":0.8`, `"probability":"yes"`, 1),
	} {
		_, err := DecodeResponse(req, []byte(body))
		require.Error(t, err, body)
	}
	_, err = DecodeResponse(req, []byte(strings.Replace(valid, `"type":"predicate","name":null,"probability":0.8`, `"type":"refusal","name":null`, 1)))
	require.NoError(t, err)
}

func TestDecodeResponsePreservesTypedChoicesAndRepeatedNames(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"gpt-6-luna","input":"x","questions":[{"type":"choice","name":"same","instructions":"x","choices":[{"value":true},{"value":"true"}]},{"type":"choice","name":"same","instructions":"x","choices":[{"value":true},{"value":"true"}]}]}`))
	require.NoError(t, err)
	body := []byte(`{"model":"gpt-6-luna","answers":[{"type":"choice","name":"same","choice":true,"confidence":0.9,"probabilities":[{"value":true,"probability":0.9},{"value":"true","probability":0.1}]},{"type":"choice","name":"same","choice":"true","confidence":0.8,"probabilities":[{"value":true,"probability":0.2},{"value":"true","probability":0.8}]}],"usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}}}`)
	_, err = DecodeResponse(req, body)
	require.NoError(t, err)
	_, err = DecodeResponse(req, []byte(strings.Replace(string(body), `"probability":0.9`, `"probability":-0.9`, 1)))
	require.Error(t, err)
}

func TestDecodeResponseScoreUsesNumericLevelDistribution(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"gpt-6-luna","input":"x","questions":[{"type":"score","instructions":"severity","levels":[{"label":"low"},{"label":"high"}]}]}`))
	require.NoError(t, err)
	valid := `{"model":"gpt-6-luna","answers":[{"type":"score","name":null,"score":0.7,"confidence":0.8,"probabilities":[{"value":0,"label":"low","probability":0.3},{"value":1,"label":"high","probability":0.7}]}],"usage":{"input_tokens":3,"output_tokens":0,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}}}`
	_, err = DecodeResponse(req, []byte(valid))
	require.NoError(t, err)
	_, err = DecodeResponse(req, []byte(strings.Replace(valid, `"value":1`, `"value":"1"`, 1)))
	require.Error(t, err)
}
