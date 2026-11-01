// Package bailiantts 定义百炼语音接口的模型能力、请求映射和用量解析。
package bailiantts

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	SpeechPath    = "/api/v1/services/audio/tts/SpeechSynthesizer"
	QwenPath      = "/api/v1/services/aigc/multimodal-generation/generation"
	InferencePath = "/api-ws/v1/inference"
	RealtimePath  = "/api-ws/v1/realtime"
)

// Model 描述一个模型的协议和计量单位；不会把 next 的完整场景生成伪装成实时朗读。
type Model struct {
	ID         string
	Characters bool
	Next       bool
	Qwen       bool
	Realtime   bool
}

// Lookup 仅接受已核对的模型族；未知模型在上游调用前明确拒绝。
func Lookup(id string) (Model, bool) {
	m := Model{ID: id}
	switch {
	case id == "qwen-audio-3.1-tts-next":
		m.Next = true
	case id == "qwen-audio-3.1-tts-flash":
	case id == "qwen-audio-3.1-tts-plus", id == "qwen-audio-3.0-tts-plus", id == "qwen-audio-3.0-tts-flash":
		m.Characters = true
	case id == "qwen3-tts-flash", strings.HasPrefix(id, "qwen3-tts-flash-realtime"):
		m.Qwen, m.Characters = true, true
		m.Realtime = strings.Contains(id, "realtime")
	case id == "cosyvoice-v2", id == "cosyvoice-v3-plus", id == "cosyvoice-v3-flash", id == "cosyvoice-v3.5-plus", id == "cosyvoice-v3.5-flash":
		m.Characters = true
	default:
		return Model{}, false
	}
	return m, true
}

// Request 是已转换为百炼原生格式的请求；Body 保留原生扩展参数。
type Request struct {
	Model  Model
	Body   []byte
	Path   string
	Text   string
	Format string
	Stream bool
}

// Prepare 将统一 speech 或原生 HTTP 请求转换并校验为确定的上游协议。
func Prepare(body []byte, compatible bool, modelID, nativePath string, sse bool) (*Request, error) {
	m, ok := Lookup(modelID)
	if !ok || m.Realtime {
		return nil, errors.New("model does not support Bailian HTTP speech")
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, errors.New("invalid speech JSON")
	}
	if obj == nil {
		return nil, errors.New("speech request is required")
	}
	stream := sse
	if v, exists := obj["stream"]; exists {
		b, valid := v.(bool)
		if !valid {
			return nil, errors.New("stream must be a boolean")
		}
		stream = stream || b
	}
	var input map[string]any
	if compatible {
		text, _ := obj["input"].(string)
		input = map[string]any{}
		if params, exists := obj["bailian_parameters"]; exists {
			var valid bool
			input, valid = params.(map[string]any)
			if !valid {
				return nil, errors.New("bailian_parameters must be an object")
			}
		}
		if m.Next {
			input["text_prompt"] = text
		} else {
			input["text"] = text
			input["voice"] = obj["voice"]
		}
		if v, exists := obj["speed"]; exists {
			input["rate"] = v
		}
		if v, exists := obj["instructions"]; exists {
			input["instruction"] = v
		}
		if v, exists := obj["response_format"]; exists {
			input["format"] = v
		}
		for k := range obj {
			switch k {
			case "model", "input", "voice", "speed", "instructions", "response_format", "stream", "bailian_parameters":
			default:
				return nil, fmt.Errorf("unsupported speech parameter: %s", k)
			}
		}
		if m.Next {
			if v, exists := obj["voice"]; exists && v != "" {
				return nil, errors.New("next uses references instead of a voice ID")
			}
			if _, exists := obj["instructions"]; exists {
				return nil, errors.New("next instructions must be included in input")
			}
		}
		obj = map[string]any{"input": input}
	} else {
		input, ok = obj["input"].(map[string]any)
		if !ok {
			return nil, errors.New("input must be an object")
		}
		delete(obj, "stream")
	}
	textKey := "text"
	if m.Next {
		textKey = "text_prompt"
	}
	text, _ := input[textKey].(string)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("input.%s is required", textKey)
	}
	if !m.Next {
		if v, ok := input["voice"].(string); !ok || strings.TrimSpace(v) == "" {
			return nil, errors.New("voice is required")
		}
	}
	if m.Next && stream {
		return nil, errors.New("next supports non-streaming audio generation only")
	}
	format, _ := input["format"].(string)
	if value, exists := input["format"]; exists {
		if _, ok := value.(string); !ok {
			return nil, errors.New("audio format must be a string")
		}
	}
	if format == "" {
		if m.Next || m.Qwen {
			format = "wav"
		} else {
			format = "mp3"
		}
	}
	if m.Qwen {
		if format != "wav" {
			return nil, errors.New("qwen3-tts-flash HTTP only supports wav output")
		}
		if _, exists := input["rate"]; exists {
			return nil, errors.New("qwen3-tts-flash HTTP does not support speed")
		}
		delete(input, "format")
	} else {
		switch format {
		case "wav", "mp3", "pcm":
		case "opus":
			if m.Next {
				return nil, errors.New("next does not support opus")
			}
		default:
			return nil, errors.New("unsupported audio format")
		}
		input["format"] = format
	}
	if m.Next && len([]rune(text)) > 3000 {
		return nil, errors.New("next input exceeds 3000 characters")
	}
	if m.Qwen && len([]rune(text)) > 600 {
		return nil, errors.New("qwen3-tts-flash input exceeds 600 characters")
	}
	if rate, exists := input["rate"]; exists {
		v, valid := rate.(float64)
		if !valid || v < .5 || v > 2 {
			return nil, errors.New("speed must be between 0.5 and 2")
		}
	}
	path := SpeechPath
	if m.Qwen {
		path = QwenPath
	}
	if nativePath != "" && nativePath != path {
		return nil, errors.New("model is not supported on this native endpoint")
	}
	obj["model"] = modelID
	encoded, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return &Request{Model: m, Body: encoded, Path: path, Text: text, Format: format, Stream: stream}, nil
}

// Usage 保留上游累计计量值，Present 区分缺失字段与明确的零。
type Usage struct {
	InputTokens  int
	OutputTokens int
	Characters   int
	Present      bool
	Invalid      bool
	RequestID    string
}

// Observe 取累计值的最大值，不把 SSE/WS 的重复累计用量相加。
func (u *Usage) Observe(body []byte, m Model) {
	j := gjson.ParseBytes(body)
	if id := j.Get("request_id").String(); id != "" {
		u.RequestID = id
	}
	v := j.Get("usage")
	if !v.Exists() {
		v = j.Get("payload.usage")
	}
	if !v.Exists() {
		v = j.Get("response.usage")
	}
	if !v.Exists() {
		return
	}
	validCounter := func(field string) bool {
		value := v.Get(field)
		return value.Type == gjson.Number && value.Int() >= 0 && value.Float() == float64(value.Int())
	}
	if m.Characters && v.Get("characters").Exists() && !validCounter("characters") {
		u.Invalid = true
		return
	}
	if !m.Characters && ((v.Get("input_tokens").Exists() && !validCounter("input_tokens")) || (v.Get("output_tokens").Exists() && !validCounter("output_tokens"))) {
		u.Invalid = true
		return
	}
	if m.Characters {
		u.Present = u.Present || v.Get("characters").Exists()
	} else {
		u.Present = u.Present || (v.Get("input_tokens").Exists() && v.Get("output_tokens").Exists())
	}
	u.InputTokens = max(u.InputTokens, int(v.Get("input_tokens").Int()))
	u.OutputTokens = max(u.OutputTokens, int(v.Get("output_tokens").Int()))
	u.Characters = max(u.Characters, int(v.Get("characters").Int()))
}

// MIME 返回未经转码的实际输出媒体类型。
func MIME(format string) string {
	switch format {
	case "wav":
		return "audio/wav"
	case "mp3":
		return "audio/mpeg"
	case "opus":
		return "audio/ogg"
	default:
		return "audio/pcm"
	}
}

// AuditText 包含朗读文本、生成指令及文本替换规则，不把参考音频 Base64 送入文本审核。
func AuditText(body []byte) string {
	var parts []string
	for _, path := range []string{"input", "input.text", "input.text_prompt", "instructions", "input.instruction", "input.hot_fix", "bailian_parameters.instruction", "bailian_parameters.hot_fix"} {
		v := gjson.GetBytes(body, path)
		if path == "input" && v.IsObject() {
			continue
		}
		if value := v.String(); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "\n")
}
