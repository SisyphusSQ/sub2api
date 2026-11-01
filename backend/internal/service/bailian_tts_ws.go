package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bailiantts"
	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	ws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ProxyBailianTTS 保留原生任务/会话协议及二进制帧，每段文本先授权，每项任务独立结算。
func (s *OpenAIGatewayService) ProxyBailianTTS(ctx context.Context, c *gin.Context, client *ws.Conn, account *Account, model string, first []byte, apiKey *APIKey, mapping ChannelMappingResult, authorizeText func(string) error, record func(*OpenAIForwardResult) error) (resultErr error) {
	forward := model
	if mapping.Mapped {
		forward = mapping.MappedModel
	}
	upstreamModel := resolveOpenAIForwardModel(account, forward, "")
	info, ok := bailiantts.Lookup(upstreamModel)
	if !ok || info.Next || (info.Qwen && !info.Realtime) {
		return errors.New("model does not support Bailian realtime speech")
	}
	path := bailiantts.InferencePath
	if info.Qwen {
		path = bailiantts.RealtimePath
	}
	if c.Request.URL.Path != path {
		return errors.New("model is not supported on this realtime endpoint")
	}
	pricingModel := upstreamModel
	switch mapping.BillingModelSource {
	case BillingModelSourceRequested:
		pricingModel = model
	case BillingModelSourceChannelMapped:
		pricingModel = forward
	}
	pricing, err := s.resolveBailianPricing(ctx, apiKey, pricingModel, info.Characters)
	if err != nil {
		return err
	}
	credential, err := resolveCredentialAccount(ctx, s.accountRepo, account)
	if err != nil {
		return err
	}
	target, err := s.BailianTTSURL(credential, path)
	if err != nil {
		return err
	}
	u, err := url.Parse(target)
	if err != nil {
		return errors.New("invalid Bailian realtime URL")
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	if info.Qwen {
		q := u.Query()
		q.Set("model", upstreamModel)
		u.RawQuery = q.Encode()
	}
	key := strings.TrimSpace(credential.GetCredential("api_key"))
	if key == "" {
		return errors.New("bailian account is missing API key")
	}
	headers := http.Header{"Authorization": []string{"Bearer " + key}}
	if workspace := credential.GetCredential("workspace_id"); workspace != "" {
		headers.Set("X-DashScope-WorkSpace", workspace)
	}
	proxy := ""
	if credential.Proxy != nil {
		proxy = credential.Proxy.URL()
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, 12*time.Second)
	up, status, _, err := s.getOpenAIWSPassthroughDialer().Dial(dialCtx, u.String(), headers, proxy)
	cancelDial()
	if err != nil {
		return fmt.Errorf("bailian WebSocket handshake failed (HTTP %d)", status)
	}
	defer func() { _ = up.Close() }()
	framed, ok := up.(openaiwsv2.FrameConn)
	if !ok {
		return errors.New("bailian transport does not support binary frames")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// 读取者只提交有界消息；状态、审核、预留和结算由当前 goroutine 串行维护。
	type frame struct {
		kind ws.MessageType
		body []byte
		err  error
	}
	fromClient := make(chan frame, 1)
	fromUpstream := make(chan frame, 1)
	go func() {
		for {
			kind, body, err := client.Read(ctx)
			select {
			case fromClient <- frame{kind, body, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		for {
			kind, body, err := framed.ReadFrame(ctx)
			select {
			case fromUpstream <- frame{kind, body, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	started := time.Now()
	sessionID := generateRequestID()
	sequence := 0
	usage := bailiantts.Usage{}
	taskID := ""
	active := false
	settle := func() error {
		if !active {
			return nil
		}
		// task_id 来自客户端；独立合成使用独立编号，避免复用 task_id 绕过扣费。
		usage.RequestID = fmt.Sprintf("%s:%d", sessionID, sequence)
		r := bailianResult(model, upstreamModel, path, true, started, pricing, usage)
		r.BillingModel = pricingModel
		r.ClientDisconnect = ctx.Err() != nil
		err := record(r)
		active = false
		usage = bailiantts.Usage{}
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, settle()) }()
	send := func(body []byte) error {
		if !gjson.ValidBytes(body) {
			return errors.New("realtime input must be a JSON event")
		}
		var canonical map[string]any
		if err := json.Unmarshal(body, &canonical); err != nil || canonical == nil {
			return errors.New("realtime event must be an object")
		}
		var err error
		body, err = json.Marshal(canonical)
		if err != nil {
			return err
		}
		text := ""
		if info.Qwen {
			if gjson.GetBytes(body, "type").String() == "input_text_buffer.append" {
				text = gjson.GetBytes(body, "text").String()
			}
			if v := gjson.GetBytes(body, "session.model").String(); v != "" && v != model && v != upstreamModel {
				return errors.New("model changes require a new connection")
			}
		} else {
			if gjson.GetBytes(body, "header.action").String() == "run-task" {
				if active {
					return errors.New("finish the active task before starting another")
				}
				if m := gjson.GetBytes(body, "payload.model").String(); m != model && m != upstreamModel {
					return errors.New("model changes require a new connection")
				}
				taskID = gjson.GetBytes(body, "header.task_id").String()
				if taskID == "" {
					return errors.New("task_id is required")
				}
				active = true
				sequence++
				started = time.Now()
				usage = bailiantts.Usage{RequestID: taskID}
				var e error
				body, e = sjson.SetBytes(body, "payload.model", upstreamModel)
				if e != nil {
					return e
				}
			} else if gjson.GetBytes(body, "header.task_id").String() != taskID {
				return errors.New("task_id does not match active task")
			}
			text = gjson.GetBytes(body, "payload.input.text").String()
		}
		if text != "" {
			if err := authorizeText(text); err != nil {
				return err
			}
		}
		for _, path := range []string{"payload.parameters.instruction", "payload.parameters.hot_fix", "session.instructions", "session.instruction"} {
			if instruction := gjson.GetBytes(body, path).String(); instruction != "" {
				if err := authorizeText(instruction); err != nil {
					return err
				}
			}
		}
		return framed.WriteFrame(ctx, ws.MessageText, body)
	}
	if len(first) > 0 {
		if err := send(first); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f := <-fromClient:
			if f.err != nil {
				if !active && ws.CloseStatus(f.err) == ws.StatusNormalClosure {
					return nil
				}
				return f.err
			}
			if f.kind != ws.MessageText {
				return errors.New("speech events must be JSON text frames")
			}
			if err := send(f.body); err != nil {
				return err
			}
		case f := <-fromUpstream:
			if f.err != nil {
				return f.err
			}
			kind := ""
			failed := false
			if f.kind == ws.MessageText {
				kind = gjson.GetBytes(f.body, "type").String()
				if info.Qwen && kind == "response.created" {
					if active {
						if err := settle(); err != nil {
							return err
						}
					}
					active = true
					sequence++
					started = time.Now()
					taskID = gjson.GetBytes(f.body, "response.id").String()
					usage = bailiantts.Usage{RequestID: taskID}
				}
				usage.Observe(f.body, info)
				if usage.Invalid {
					return errors.New("bailian realtime returned invalid usage")
				}
				event := gjson.GetBytes(f.body, "header.event").String()
				terminal := event == "task-finished" || kind == "response.done"
				failed = event == "task-failed" || kind == "error"
				if terminal && (!active || !usage.Present) {
					return errors.New("bailian realtime completion has no billable usage")
				}
				if terminal {
					if err := settle(); err != nil {
						return err
					}
				}
			}
			if err := client.Write(ctx, f.kind, f.body); err != nil {
				return err
			}
			if failed {
				return errors.New("bailian realtime task failed")
			}
			if kind == "session.finished" {
				return nil
			}
		}
	}
}
