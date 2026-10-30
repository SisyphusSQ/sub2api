package openai_decisions

import "encoding/json"

// ExtractEvidence 提取全部共享输入和问题内容供既有审计使用，图片单独传递而不作为文本持久化。
func ExtractEvidence(body []byte) (texts, images []string) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, nil
	}
	var collect func(json.RawMessage)
	collect = func(raw json.RawMessage) {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			texts = append(texts, text)
			return
		}
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) == nil {
			for _, item := range items {
				collect(item)
			}
			return
		}
		var part map[string]json.RawMessage
		if json.Unmarshal(raw, &part) != nil {
			return
		}
		var kind string
		_ = json.Unmarshal(part["type"], &kind)
		switch kind {
		case "input_image":
			if json.Unmarshal(part["image_url"], &text) == nil {
				images = append(images, text)
			}
		case "input_json":
			if data, ok := part["data"]; ok {
				texts = append(texts, string(data))
			}
		default:
			if content, ok := part["content"]; ok {
				collect(content)
			}
			if value, ok := part["text"]; ok {
				collect(value)
			}
		}
	}
	collect(root["input"])
	var questions []map[string]json.RawMessage
	_ = json.Unmarshal(root["questions"], &questions)
	for _, question := range questions {
		collect(question["name"])
		collect(question["instructions"])
		for _, key := range []string{"choices", "levels"} {
			var entries []map[string]json.RawMessage
			_ = json.Unmarshal(question[key], &entries)
			for _, entry := range entries {
				collect(entry["description"])
				collect(entry["label"])
				if value, ok := entry["value"]; ok {
					texts = append(texts, string(value))
				}
			}
		}
	}
	return texts, images
}
