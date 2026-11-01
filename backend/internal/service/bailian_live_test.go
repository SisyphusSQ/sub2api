//go:build bailian_live

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ws "github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestBailianLiveWebSocket 仅在显式提供隔离网关 Key 时运行，会产生真实百炼调用费用。
func TestBailianLiveWebSocket(t *testing.T) {
	base := os.Getenv("BAILIAN_LIVE_BASE_URL")
	key := os.Getenv("BAILIAN_LIVE_KEY")
	out := os.Getenv("BAILIAN_LIVE_OUTPUT_DIR")
	if base == "" || key == "" || out == "" {
		t.Skip("explicit live gateway credentials are required")
	}
	for _, model := range strings.Split(os.Getenv("BAILIAN_LIVE_WS_MODELS"), ",") {
		if model == "" {
			continue
		}
		t.Run(model, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			qwen := strings.HasPrefix(model, "qwen3-")
			path := "/api-ws/v1/inference"
			if qwen {
				path = "/api-ws/v1/realtime?model=" + model
			}
			conn, _, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+path, &ws.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + key}}})
			require.NoError(t, err)
			defer func() { _ = conn.CloseNow() }()
			conn.SetReadLimit(8 << 20)
			send := func(v any) {
				b, e := json.Marshal(v)
				require.NoError(t, e)
				require.NoError(t, conn.Write(ctx, ws.MessageText, b))
			}
			id := uuid.NewString()
			started := time.Now()
			firstMs := int64(-1)
			var audio []byte
			var events []string
			taskEvent := func(action string, input any) any {
				return map[string]any{"header": map[string]any{"action": action, "task_id": id, "streaming": "duplex"}, "payload": map[string]any{"input": input}}
			}
			voice := "longanhuan_v3.1"
			if strings.HasPrefix(model, "cosyvoice-") {
				voice = "longanyang"
			}
			if !qwen {
				send(map[string]any{"header": map[string]any{"action": "run-task", "task_id": id, "streaming": "duplex"}, "payload": map[string]any{"task_group": "audio", "task": "tts", "function": "SpeechSynthesizer", "model": model, "parameters": map[string]any{"voice": voice, "format": "mp3", "sample_rate": 24000}, "input": map[string]any{}}})
			}
			done := false
			for !done {
				kind, b, e := conn.Read(ctx)
				require.NoError(t, e, "events: %v", events)
				if kind == ws.MessageBinary {
					if firstMs < 0 {
						firstMs = time.Since(started).Milliseconds()
					}
					audio = append(audio, b...)
					continue
				}
				typ := gjson.GetBytes(b, "type").String()
				event := gjson.GetBytes(b, "header.event").String()
				events = append(events, typ+event)
				require.NotEqual(t, "error", typ, "%s", b)
				require.NotEqual(t, "task-failed", event, "%s", b)
				if event == "task-started" {
					send(taskEvent("continue-task", map[string]string{"text": "你好，百炼实时语音测试。"}))
					send(taskEvent("finish-task", map[string]any{}))
				}
				if typ == "session.created" {
					send(map[string]any{"type": "session.update", "session": map[string]any{"mode": "commit", "voice": "Cherry", "response_format": "pcm", "sample_rate": 24000}})
				}
				if typ == "session.updated" {
					send(map[string]any{"type": "input_text_buffer.append", "text": "你好，实时语音测试。"})
					send(map[string]any{"type": "input_text_buffer.commit"})
					send(map[string]any{"type": "session.finish"})
				}
				if typ == "response.audio.delta" {
					part, e := base64.StdEncoding.DecodeString(gjson.GetBytes(b, "delta").String())
					require.NoError(t, e)
					if firstMs < 0 {
						firstMs = time.Since(started).Milliseconds()
					}
					audio = append(audio, part...)
				}
				if event == "task-finished" || typ == "session.finished" {
					done = true
				}
			}
			require.NotEmpty(t, audio)
			ext := ".mp3"
			if qwen {
				ext = ".pcm"
			}
			require.NoError(t, os.WriteFile(filepath.Join(out, model+"-ws"+ext), audio, 0o600))
			summary := map[string]any{"model": model, "audio_bytes": len(audio), "first_ms": firstMs, "total_ms": time.Since(started).Milliseconds(), "events": events}
			b, e := json.MarshalIndent(summary, "", "  ")
			require.NoError(t, e)
			require.NoError(t, os.WriteFile(filepath.Join(out, model+"-ws.json"), b, 0o600))
			fmt.Println(string(b))
			_ = conn.Close(ws.StatusNormalClosure, "complete")
		})
	}
}
