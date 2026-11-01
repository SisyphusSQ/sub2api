package service

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bailiantts"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func (s *AccountTestService) testBailianTTS(c *gin.Context, account *Account, model, prompt, mode, voice string) error {
	if model == "" {
		model = "qwen-audio-3.1-tts-plus"
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "你好，这是百炼语音连接测试。"
	}
	input := map[string]any{"text": prompt, "voice": "longanhuan_v3.1", "format": "mp3"}
	if strings.HasPrefix(model, "cosyvoice-") {
		input["voice"] = "longanyang"
	}
	if strings.Contains(model, "-next") {
		input = map[string]any{"text_prompt": prompt, "format": "mp3"}
	}
	if strings.HasPrefix(model, "qwen3-tts-") {
		input = map[string]any{"text": prompt, "voice": "Cherry"}
	}
	if voice != "" && !strings.Contains(model, "-next") {
		input["voice"] = voice
	}
	body, _ := json.Marshal(map[string]any{"model": model, "input": input})
	stream := mode == "tts_stream"
	prepared, err := bailiantts.Prepare(body, false, model, "", stream)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	base := strings.TrimRight(account.GetCredential("base_url"), "/")
	if base == "" {
		base = "https://dashscope.aliyuncs.com"
	}
	validated, err := (&OpenAIGatewayService{cfg: s.cfg}).validateUpstreamBaseURL(base)
	if err != nil {
		return s.sendErrorAndEnd(c, "Invalid Bailian base URL")
	}
	s.prepareGrokTestSSE(c)
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, strings.TrimRight(validated, "/")+prepared.Path, bytes.NewReader(prepared.Body))
	if err != nil {
		return s.sendErrorAndEnd(c, "Unable to create speech request")
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("X-DashScope-SSE", "enable")
	}
	req.Header.Set("Authorization", "Bearer "+account.GetCredential("api_key"))
	if workspace := account.GetCredential("workspace_id"); workspace != "" {
		req.Header.Set("X-DashScope-WorkSpace", workspace)
	}
	proxy := ""
	if account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	started := time.Now()
	resp, err := s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, "Bailian connection failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if stream && resp.StatusCode == 200 {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64<<10), int(resolveUpstreamResponseReadLimit(s.cfg)))
		var audio bytes.Buffer
		var usage bailiantts.Usage
		complete := false
		first := true
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			if !gjson.ValidBytes(payload) {
				return s.sendErrorAndEnd(c, "Invalid Bailian stream event")
			}
			usage.Observe(payload, prepared.Model)
			if chunk := gjson.GetBytes(payload, "output.audio.data").String(); chunk != "" {
				decoded, e := base64.StdEncoding.DecodeString(chunk)
				if e != nil {
					return s.sendErrorAndEnd(c, "Invalid Bailian audio")
				}
				if audio.Len()+len(decoded) > 4<<20 {
					return s.sendErrorAndEnd(c, "Audio exceeds the 4 MiB preview limit")
				}
				_, _ = audio.Write(decoded)
				if first {
					s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("First audio: %d ms\n", time.Since(started).Milliseconds())})
					first = false
				}
			}
			complete = complete || gjson.GetBytes(payload, "output.finish_reason").String() == "stop"
		}
		if scanner.Err() != nil || !complete || !usage.Present || usage.Invalid || audio.Len() == 0 {
			return s.sendErrorAndEnd(c, "Bailian stream ended without audio, usage or completion")
		}
		s.sendEvent(c, TestEvent{Type: "audio", AudioURL: "data:" + bailiantts.MIME(prepared.Format) + ";base64," + base64.StdEncoding.EncodeToString(audio.Bytes()), MimeType: bailiantts.MIME(prepared.Format)})
		s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("characters=%d input_tokens=%d output_tokens=%d\n", usage.Characters, usage.InputTokens, usage.OutputTokens)})
		s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
		return nil
	}
	data, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, nil)
	if err != nil {
		return s.sendErrorAndEnd(c, "Unable to read Bailian response")
	}
	if resp.StatusCode != 200 {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Bailian returned HTTP %d", resp.StatusCode))
	}
	audioURL := gjson.GetBytes(data, "output.audio.url").String()
	if audioURL == "" {
		return s.sendErrorAndEnd(c, "Bailian returned no audio URL")
	}
	s.sendEvent(c, TestEvent{Type: "audio", AudioURL: audioURL, MimeType: bailiantts.MIME(prepared.Format)})
	s.sendEvent(c, TestEvent{Type: "content", Text: "Bailian TTS completed. Audio URL expires after 24 hours; no permanent archive was created."})
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}
