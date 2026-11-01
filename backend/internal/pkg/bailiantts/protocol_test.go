package bailiantts

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPrepareSpeechProtocols(t *testing.T) {
	for _, tc := range []struct {
		name, model, body, path, text string
		compat, stream, wantError     bool
	}{
		{"plus", "qwen-audio-3.1-tts-plus", `{"input":"你好","voice":"longanhuan_v3.1","response_format":"mp3","stream":true}`, SpeechPath, "你好", true, true, false},
		{"next", "qwen-audio-3.1-tts-next", `{"input":"一位女性说你好","response_format":"wav"}`, SpeechPath, "一位女性说你好", true, false, false},
		{"qwen", "qwen3-tts-flash", `{"input":"你好","voice":"Cherry"}`, QwenPath, "你好", true, false, false},
		{"native", "cosyvoice-v3-flash", `{"input":{"text":"你好","voice":"longanyang","word_timestamp_enabled":true}}`, SpeechPath, "你好", false, true, false},
		{"next stream", "qwen-audio-3.1-tts-next", `{"input":"你好","stream":true}`, "", "", true, false, true},
		{"next voice", "qwen-audio-3.1-tts-next", `{"input":"你好","voice":"Cherry"}`, "", "", true, false, true},
		{"qwen speed", "qwen3-tts-flash", `{"input":"你好","voice":"Cherry","speed":1.2}`, "", "", true, false, true},
		{"unknown parameter", "qwen-audio-3.1-tts-plus", `{"input":"你好","voice":"A","temperature":1}`, "", "", true, false, true},
		{"voice required", "qwen-audio-3.1-tts-plus", `{"input":"你好"}`, "", "", true, false, true},
		{"wrong endpoint", "qwen3-tts-flash", `{"input":{"text":"你好","voice":"Cherry"}}`, SpeechPath, "", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Prepare([]byte(tc.body), tc.compat, tc.model, func() string {
				if tc.compat {
					return ""
				}
				return tc.path
			}(), tc.stream)
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.path, r.Path)
			require.Equal(t, tc.text, r.Text)
			require.Equal(t, tc.model, gjson.GetBytes(r.Body, "model").String())
			if tc.name == "native" {
				require.True(t, gjson.GetBytes(r.Body, "input.word_timestamp_enabled").Bool())
			}
		})
	}
}

func TestUsageCumulativeAndUnits(t *testing.T) {
	plus, _ := Lookup("qwen-audio-3.1-tts-plus")
	var u Usage
	u.Observe([]byte(`{"usage":{"characters":12,"input_tokens":4,"output_tokens":8}}`), plus)
	u.Observe([]byte(`{"usage":{"characters":12,"input_tokens":4,"output_tokens":8}}`), plus)
	u.Observe([]byte(`{"usage":{"characters":20,"input_tokens":6,"output_tokens":10}}`), plus)
	require.True(t, u.Present)
	require.Equal(t, 20, u.Characters)
	require.Equal(t, 10, u.OutputTokens)
	flash, _ := Lookup("qwen-audio-3.1-tts-flash")
	u = Usage{}
	u.Observe([]byte(`{"usage":{"duration":12}}`), flash)
	require.False(t, u.Present)
	u.Observe([]byte(`{"response":{"usage":{"input_tokens":0,"output_tokens":0}}}`), flash)
	require.True(t, u.Present)
}

func TestAuditTextIncludesInstructionsAndExcludesAudio(t *testing.T) {
	text := AuditText([]byte(`{"input":{"text":"hello","instruction":"speak softly","hot_fix":{"replace":[{"hello":"world"}]},"references":[{"audio_data":"private-audio"}]}}`))
	require.Contains(t, text, "hello")
	require.Contains(t, text, "speak softly")
	require.Contains(t, text, "world")
	require.NotContains(t, text, "private-audio")
}

func TestInvalidUsageIsNotFree(t *testing.T) {
	model, _ := Lookup("qwen-audio-3.1-tts-flash")
	var u Usage
	u.Observe([]byte(`{"usage":{"input_tokens":-1,"output_tokens":0}}`), model)
	require.False(t, u.Present)
	require.True(t, u.Invalid)
}
