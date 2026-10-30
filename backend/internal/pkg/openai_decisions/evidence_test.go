package openai_decisions

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEvidenceIncludesQuestionsAndStructuredInputWithoutImageBytes(t *testing.T) {
	body := []byte(`{"model":"model-metadata","prompt_cache_key":"cache-metadata","input":[{"role":"user","content":[{"type":"input_text","text":"shared evidence"},{"type":"input_json","data":{"risk":"json evidence","count":7}},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}],"questions":[{"type":"choice","name":"category","instructions":[{"type":"input_text","text":"question evidence"},{"type":"input_image","image_url":"https://example.com/q.png"}],"choices":[{"value":true,"description":"choice evidence"}]},{"type":"score","instructions":"score evidence","levels":[{"label":"level label","description":"level evidence"}]}]}`)
	texts, images := ExtractEvidence(body)
	joined := strings.Join(texts, "\n")
	for _, text := range []string{"shared evidence", "json evidence", "question evidence", "choice evidence", "score evidence", "level label", "level evidence"} {
		require.Contains(t, joined, text)
	}
	require.NotContains(t, joined, "AAAA")
	require.NotContains(t, joined, "model-metadata")
	require.NotContains(t, joined, "cache-metadata")
	require.Equal(t, []string{"data:image/png;base64,AAAA", "https://example.com/q.png"}, images)
}
