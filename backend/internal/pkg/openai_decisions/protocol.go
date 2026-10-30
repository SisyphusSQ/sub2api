// Package openai_decisions 解析原生 Decisions 的路由信息和真实用量，原始报文由转发层保留。
package openai_decisions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const Endpoint = "/v1/decisions"

type Question struct {
	Type         string          `json:"type"`
	Name         *string         `json:"name"`
	Instructions json.RawMessage `json:"instructions"`
	Choices      []struct {
		Value any `json:"value"`
	} `json:"choices"`
	Levels []json.RawMessage `json:"levels"`
}

type Request struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Questions          []Question      `json:"questions"`
	PromptCacheOptions *struct {
		Mode string `json:"mode"`
		TTL  string `json:"ttl"`
	} `json:"prompt_cache_options"`
}

type Usage struct {
	InputTokens      int
	OutputTokens     int
	CachedTokens     int
	CacheWriteTokens int
}

// ParseRequest 只校验本地路由和结算必需的结构；原生扩展字段继续交给上游处理。
func ParseRequest(body []byte) (*Request, error) {
	fields, err := requestFields(body)
	if err != nil {
		return nil, err
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, errors.New("request must be a Decisions JSON object")
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, errors.New("model is required")
	}
	input := bytes.TrimSpace(req.Input)
	if len(input) == 0 || (input[0] != '"' && input[0] != '[') {
		return nil, errors.New("input must be a string or an array")
	}
	if len(req.Questions) < 1 || len(req.Questions) > 400 {
		return nil, errors.New("questions must contain 1 to 400 entries")
	}
	for i, question := range req.Questions {
		switch question.Type {
		case "predicate", "choice", "score":
		default:
			return nil, fmt.Errorf("question %d has an unsupported type", i)
		}
		for _, choice := range question.Choices {
			switch choice.Value.(type) {
			case string, bool:
			default:
				return nil, fmt.Errorf("question %d choices must use string or boolean values", i)
			}
		}
		instructions := bytes.TrimSpace(question.Instructions)
		if len(instructions) == 0 || (instructions[0] != '"' && instructions[0] != '[') {
			return nil, fmt.Errorf("question %d instructions must be a string or an array", i)
		}
	}
	if options := req.PromptCacheOptions; options != nil {
		if options.TTL != "" && options.TTL != "30m" {
			return nil, errors.New("prompt_cache_options.ttl only supports 30m")
		}
		if options.Mode != "" && options.Mode != "implicit" && options.Mode != "explicit" {
			return nil, errors.New("invalid prompt_cache_options.mode")
		}
	}
	if _, exists := fields["stream"]; exists {
		return nil, errors.New("decisions does not support stream")
	}
	return &req, nil
}

// requestFields 拒绝顶层重复键：权限中间件的 gjson 和上游 JSON 解码不能看到不同的模型。
func requestFields(body []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("request must be a Decisions JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, errors.New("invalid Decisions request JSON")
		}
		name, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid Decisions request parameter")
		}
		if _, exists := fields[name]; exists {
			return nil, errors.New("duplicate top-level Decisions parameters are not allowed")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("invalid Decisions request JSON")
		}
		fields[name] = value
	}
	return fields, nil
}

// DecodeResponse 不估算用量；缺失、负值或相互矛盾的 token 不能进入正常账单。
func DecodeResponse(req *Request, body []byte) (Usage, error) {
	var response struct {
		Model   string            `json:"model"`
		Answers []json.RawMessage `json:"answers"`
		Usage   *struct {
			InputTokens        *int `json:"input_tokens"`
			OutputTokens       *int `json:"output_tokens"`
			InputTokensDetails *struct {
				CachedTokens     *int `json:"cached_tokens"`
				CacheWriteTokens *int `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return Usage{}, errors.New("invalid Decisions response JSON")
	}
	if req == nil || response.Model == "" || len(response.Answers) != len(req.Questions) {
		return Usage{}, errors.New("invalid Decisions response model or answers")
	}
	u := response.Usage
	if u == nil || u.InputTokens == nil || u.OutputTokens == nil || u.InputTokensDetails == nil || u.InputTokensDetails.CachedTokens == nil || u.InputTokensDetails.CacheWriteTokens == nil {
		return Usage{}, errors.New("decisions response is missing usage")
	}
	input, output := *u.InputTokens, *u.OutputTokens
	read, write := *u.InputTokensDetails.CachedTokens, *u.InputTokensDetails.CacheWriteTokens
	if input < 0 || output < 0 || read < 0 || write < 0 || read > input || write > input-read {
		return Usage{}, errors.New("invalid Decisions token usage")
	}
	for i, raw := range response.Answers {
		var answer struct {
			Type          string   `json:"type"`
			Name          *string  `json:"name"`
			Probability   *float64 `json:"probability"`
			Confidence    *float64 `json:"confidence"`
			Score         *float64 `json:"score"`
			Choice        any      `json:"choice"`
			Probabilities []struct {
				Value       json.RawMessage `json:"value"`
				Probability *float64        `json:"probability"`
			} `json:"probabilities"`
		}
		if err := json.Unmarshal(raw, &answer); err != nil {
			return Usage{}, fmt.Errorf("invalid answer %d", i)
		}
		question := req.Questions[i]
		if (question.Name == nil) != (answer.Name == nil) || (question.Name != nil && *question.Name != *answer.Name) {
			return Usage{}, fmt.Errorf("answer %d name does not match question", i)
		}
		if answer.Type == "refusal" {
			continue
		}
		if answer.Type != question.Type {
			return Usage{}, fmt.Errorf("answer %d type does not match question", i)
		}
		switch answer.Type {
		case "predicate":
			if !probabilityValid(answer.Probability) {
				return Usage{}, fmt.Errorf("invalid predicate answer %d", i)
			}
		case "choice":
			found := false
			for _, choice := range question.Choices {
				if choice.Value == answer.Choice {
					found = true
					break
				}
			}
			if !found || !probabilityValid(answer.Confidence) || len(answer.Probabilities) == 0 {
				return Usage{}, fmt.Errorf("invalid choice answer %d", i)
			}
		case "score":
			if answer.Score == nil || *answer.Score < 0 || *answer.Score > float64(len(question.Levels)-1) || !probabilityValid(answer.Confidence) || len(answer.Probabilities) == 0 {
				return Usage{}, fmt.Errorf("invalid score answer %d", i)
			}
		}
		for _, entry := range answer.Probabilities {
			if !probabilityValid(entry.Probability) {
				return Usage{}, fmt.Errorf("invalid answer %d probability", i)
			}
			if answer.Type == "score" {
				var level *int
				if json.Unmarshal(entry.Value, &level) != nil || level == nil || *level < 0 || *level >= len(question.Levels) {
					return Usage{}, fmt.Errorf("invalid answer %d level", i)
				}
			} else if answer.Type == "choice" {
				var value any
				if json.Unmarshal(entry.Value, &value) != nil {
					return Usage{}, fmt.Errorf("invalid answer %d choice", i)
				}
				found := false
				for _, choice := range question.Choices {
					if choice.Value == value {
						found = true
						break
					}
				}
				if !found {
					return Usage{}, fmt.Errorf("invalid answer %d choice", i)
				}
			}
		}
	}
	return Usage{InputTokens: input, OutputTokens: output, CachedTokens: read, CacheWriteTokens: write}, nil
}

func probabilityValid(value *float64) bool { return value != nil && *value >= 0 && *value <= 1 }
