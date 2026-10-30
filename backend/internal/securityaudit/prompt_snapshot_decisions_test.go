package securityaudit

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecisionsSnapshotIncludesQuestionAndJSONEvidence(t *testing.T) {
	snapshot, err := ExtractPromptSnapshot(Request{Protocol: "openai_decisions", Body: []byte(`{"model":"gpt-6-luna","input":[{"type":"input_json","data":{"risk":"structured evidence"}}],"questions":[{"type":"choice","instructions":"question evidence","choices":[{"value":true,"description":"choice evidence"}]}]}`)})
	require.NoError(t, err)
	for _, evidence := range []string{"structured evidence", "question evidence", "choice evidence"} {
		require.Contains(t, snapshot.ScanText, evidence)
	}
	require.NotEmpty(t, snapshot.PromptHash)
}
