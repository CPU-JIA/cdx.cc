package transform

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"cdx.cc/claude-bridge/internal/config"
	"cdx.cc/claude-bridge/internal/types"
)

const (
	roleUser      = "user"
	roleAssistant = "assistant"
	roleSystem    = "system"
	roleDeveloper = "developer"
)

func DecodeAnthropicRequest(body []byte, mode config.Mode) (types.AnthropicMessageRequest, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return types.AnthropicMessageRequest{}, err
	}

	var req types.AnthropicMessageRequest

	consume := func(key string, target any, required bool) error {
		val, ok := raw[key]
		if !ok {
			if required {
				return fmt.Errorf("missing field: %s", key)
			}
			return nil
		}
		delete(raw, key)
		if target == nil {
			return nil
		}
		if err := json.Unmarshal(val, target); err != nil {
			return fmt.Errorf("invalid %s: %w", key, err)
		}
		return nil
	}

	if err := consume("model", &req.Model, true); err != nil {
		return req, err
	}
	if err := consume("max_tokens", &req.MaxTokens, true); err != nil {
		return req, err
	}
	if err := consume("messages", &req.Messages, true); err != nil {
		return req, err
	}
	if err := consume("system", &req.System, false); err != nil {
		return req, err
	}
	if err := consume("tools", &req.Tools, false); err != nil {
		return req, err
	}
	if err := consume("tool_choice", &req.ToolChoice, false); err != nil {
		return req, err
	}
	if err := consume("stream", &req.Stream, false); err != nil {
		return req, err
	}
	if err := consume("temperature", &req.Temperature, false); err != nil {
		return req, err
	}
	if err := consume("top_p", &req.TopP, false); err != nil {
		return req, err
	}
	if err := consume("top_k", &req.TopK, false); err != nil {
		return req, err
	}
	if err := consume("stop_sequences", &req.StopSequences, false); err != nil {
		return req, err
	}
	if err := consume("metadata", &req.Metadata, false); err != nil {
		return req, err
	}
	if err := consume("thinking", &req.Thinking, false); err != nil {
		return req, err
	}
	if err := consume("speed", &req.Speed, false); err != nil {
		return req, err
	}
	if err := consume("context_management", &req.ContextManagement, false); err != nil {
		return req, err
	}

	if mode == config.ModeStrict && len(raw) > 0 {
		keys := make([]string, 0, len(raw))
		for key := range raw {
			keys = append(keys, key)
		}
		return req, fmt.Errorf("unsupported fields: %s", strings.Join(keys, ", "))
	}

	return req, nil
}

func TransformAnthropicToOpenAI(req types.AnthropicMessageRequest, mode config.Mode, modelMap map[string]config.ModelMapping) (types.OpenAIResponsesRequest, error) {
	if req.Model == "" {
		return types.OpenAIResponsesRequest{}, errors.New("model is required")
	}
	if req.MaxTokens <= 0 {
		return types.OpenAIResponsesRequest{}, errors.New("max_tokens must be positive")
	}

	// 模型映射：入站模型名 → 上游模型名
	actualModel := req.Model
	var reasoningOverride string
	if modelMap != nil {
		if mapping, ok := modelMap[req.Model]; ok {
			actualModel = mapping.UpstreamModel
			reasoningOverride = mapping.ReasoningEffort
		}
	}

	storeTrue := true
	oa := types.OpenAIResponsesRequest{
		Model:           actualModel,
		MaxOutputTokens: &req.MaxTokens,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		// Metadata 不转发：Sub2API 等代理不支持此字段，会导致 502
		// Metadata:     req.Metadata,
		Stream: req.Stream,
		Store:  &storeTrue,
	}
	if os.Getenv("DISABLE_RESPONSE_STORAGE") != "" {
		storeFalse := false
		oa.Store = &storeFalse
	}
	if len(req.StopSequences) > 0 {
		oa.Stop = append([]string{}, req.StopSequences...)
	}

	// thinking → reasoning 映射
	if len(req.Thinking) > 0 {
		reasoning, err := mapThinkingToReasoning(req.Thinking)
		if err != nil {
			if mode == config.ModeStrict {
				return oa, fmt.Errorf("invalid thinking config: %w", err)
			}
			log.Printf("WARN: skipping invalid thinking config: %v", err)
		} else if reasoning != nil {
			oa.Reasoning = reasoning
		}
	}

	// 模型映射的推理强度覆盖（优先级高于 thinking 参数推算）
	if reasoningOverride != "" {
		if oa.Reasoning == nil {
			oa.Reasoning = &types.OpenAIReasoning{Summary: "auto"}
		}
		oa.Reasoning.Effort = reasoningOverride
	}

	// /fast 模式：CC 客户端自动切换 Opus 4.6，bridge 不额外处理推理强度

	if len(req.Tools) > 0 {
		oa.Tools = make([]types.OpenAITool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			// server-side 工具：web_search → web_search（OpenAI GA 版本）
			if strings.HasPrefix(tool.Type, "web_search") {
				oa.Tools = append(oa.Tools, types.OpenAITool{
					Type: "web_search",
				})
				continue
			}
			// 普通 function 工具
			if tool.Name == "" {
				return oa, errors.New("tool.name is required")
			}
			oa.Tools = append(oa.Tools, types.OpenAITool{
				Type:        "function",
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
			})
		}
	}

	if req.TopK != nil && mode == config.ModeStrict {
		return oa, errors.New("top_k is not supported by the upstream")
	}

	if len(req.ToolChoice) > 0 {
		toolChoice, err := mapToolChoice(req.ToolChoice)
		if err != nil {
			if mode == config.ModeStrict {
				return oa, err
			}
		} else if toolChoice != nil {
			oa.ToolChoice = toolChoice
		}
	}

	if len(req.System) > 0 {
		sys, err := normalizeSystem(req.System)
		if err != nil {
			if mode == config.ModeStrict {
				return oa, err
			}
		} else if sys != "" {
			oa.Instructions = sys
		}
	}

	inputItems := make([]types.OpenAIInputItem, 0, len(req.Messages))
	for _, msg := range req.Messages {
		items, err := messageToInputItems(msg, mode)
		if err != nil {
			return oa, err
		}
		inputItems = append(inputItems, items...)
	}
	oa.Input = inputItems

	// Anthropic compaction beta → OpenAI context_management 转换
	if len(req.ContextManagement) > 0 {
		oaCtxMgmt := mapContextManagement(req.ContextManagement)
		if oaCtxMgmt != nil {
			oa.ContextManagement = oaCtxMgmt
		}
	}

	return oa, nil
}

func normalizeSystem(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return strings.TrimSpace(s), nil
	}

	var blocks []struct {
		Type         string         `json:"type"`
		Text         string         `json:"text"`
		CacheControl map[string]any `json:"cache_control,omitempty"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", errors.New("system must be a string or array of text blocks")
	}
	var parts []string
	for _, block := range blocks {
		if block.Type != "text" {
			return "", errors.New("system blocks must be text")
		}
		if block.CacheControl != nil {
			log.Printf("WARN: stripping cache_control from system block")
		}
		parts = append(parts, block.Text)
	}
	return strings.TrimSpace(strings.Join(parts, "\n")), nil
}

func messageToInputItems(msg types.AnthropicMessage, mode config.Mode) ([]types.OpenAIInputItem, error) {
	role := strings.ToLower(strings.TrimSpace(msg.Role))
	if role != roleUser && role != roleAssistant {
		return nil, fmt.Errorf("unsupported role: %s", msg.Role)
	}

	blocks, err := parseContentBlocks(msg.Content)
	if err != nil {
		return nil, err
	}

	var (
		items   []types.OpenAIInputItem
		current *types.OpenAIInputItem
		flush   = func() {
			if current != nil && len(current.Content) > 0 {
				items = append(items, *current)
			}
			current = nil
		}
	)

	for _, block := range blocks {
		switch block.Type {
		case "thinking", "signature":
			// 历史消息中的 thinking/signature 块 → 直接跳过（OpenAI 不需要）
			log.Printf("WARN: stripping %s block from history message", block.Type)
			continue
		case "text":
			if current == nil {
				current = &types.OpenAIInputItem{Type: "message", Role: role}
			}
			contentType := "input_text"
			if role == roleAssistant {
				contentType = "output_text"
			}
			current.Content = append(current.Content, types.OpenAIInputContent{
				Type: contentType,
				Text: block.Text,
			})
		case "image":
			if block.Source == nil {
				return nil, errors.New("image block missing source")
			}
			imageURL, err := imageSourceToURL(*block.Source)
			if err != nil {
				if mode == config.ModeStrict {
					return nil, err
				}
				log.Printf("WARN: skipping image block: %v", err)
				continue
			}
			if current == nil {
				current = &types.OpenAIInputItem{Type: "message", Role: role}
			}
			current.Content = append(current.Content, types.OpenAIInputContent{
				Type:     "input_image",
				ImageURL: imageURL,
			})
		case "document":
			// document 块 → best_effort 转为 input_text 占位
			if mode == config.ModeStrict {
				return nil, errors.New("document blocks are not supported by upstream")
			}
			docName := block.Name
			if docName == "" {
				docName = "unknown"
			}
			log.Printf("WARN: converting document block to text placeholder: %s", docName)
			if current == nil {
				current = &types.OpenAIInputItem{Type: "message", Role: role}
			}
			current.Content = append(current.Content, types.OpenAIInputContent{
				Type: "input_text",
				Text: fmt.Sprintf("[document: %s]", docName),
			})
		case "tool_use":
			flush()
			if block.Name == "" || block.ID == "" {
				return nil, errors.New("tool_use requires id and name")
			}
			args := strings.TrimSpace(string(block.Input))
			if args == "" {
				args = "{}"
			}
			if mode == config.ModeStrict {
				var js any
				if err := json.Unmarshal([]byte(args), &js); err != nil {
					return nil, fmt.Errorf("tool_use input must be valid JSON: %w", err)
				}
			}
			items = append(items, types.OpenAIInputItem{
				Type:      "function_call",
				CallID:    block.ID,
				Name:      block.Name,
				Arguments: args,
			})
		case "tool_result":
			flush()
			if block.ToolUseID == "" {
				return nil, errors.New("tool_result requires tool_use_id")
			}
			output, err := toolResultToOutput(block, mode)
			if err != nil {
				return nil, err
			}
			items = append(items, types.OpenAIInputItem{
				Type:   "function_call_output",
				CallID: block.ToolUseID,
				Output: output,
			})
		default:
			if mode == config.ModeStrict {
				return nil, fmt.Errorf("unsupported content block type: %s", block.Type)
			}
			log.Printf("WARN: skipping unsupported content block type: %s", block.Type)
		}
	}

	flush()
	return items, nil
}

func parseContentBlocks(raw json.RawMessage) ([]types.AnthropicContentBlock, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, err
		}
		return []types.AnthropicContentBlock{{Type: "text", Text: text}}, nil
	}
	if raw[0] != '[' {
		return nil, errors.New("content must be string or array")
	}
	var blocks []types.AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

func imageSourceToURL(src types.AnthropicImage) (string, error) {
	switch src.Type {
	case "base64":
		if src.MediaType == "" || src.Data == "" {
			return "", errors.New("base64 image requires media_type and data")
		}
		if _, err := base64.StdEncoding.DecodeString(src.Data); err != nil {
			return "", errors.New("invalid base64 image data")
		}
		return fmt.Sprintf("data:%s;base64,%s", src.MediaType, src.Data), nil
	case "url":
		if src.URL == "" {
			return "", errors.New("image url missing")
		}
		return src.URL, nil
	default:
		return "", fmt.Errorf("unsupported image source type: %s", src.Type)
	}
}

func toolResultToOutput(block types.AnthropicContentBlock, mode config.Mode) (any, error) {
	if len(block.Content) == 0 {
		return "", nil
	}

	if block.Content[0] == '"' {
		var s string
		if err := json.Unmarshal(block.Content, &s); err != nil {
			return nil, err
		}
		if block.IsError != nil && *block.IsError {
			return "ERROR: " + s, nil
		}
		return s, nil
	}

	if block.Content[0] == '[' {
		var blocks []types.AnthropicContentBlock
		if err := json.Unmarshal(block.Content, &blocks); err != nil {
			return nil, err
		}
		var outParts []string
		for _, cb := range blocks {
			switch cb.Type {
			case "text":
				outParts = append(outParts, cb.Text)
			case "image":
				if mode == config.ModeStrict {
					return nil, errors.New("tool_result image blocks are not supported in strict mode")
				}
				outParts = append(outParts, "[image]")
			default:
				if mode == config.ModeStrict {
					return nil, fmt.Errorf("unsupported tool_result block: %s", cb.Type)
				}
			}
		}
		joined := strings.Join(outParts, "\n")
		if block.IsError != nil && *block.IsError {
			joined = "ERROR: " + joined
		}
		return joined, nil
	}

	if mode == config.ModeStrict {
		return nil, errors.New("tool_result content must be string or array")
	}
	return string(bytes.TrimSpace(block.Content)), nil
}

func mapToolChoice(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		switch s {
		case "auto":
			return "auto", nil
		case "any":
			return "required", nil
		case "none":
			return "none", nil
		default:
			return nil, fmt.Errorf("unsupported tool_choice: %s", s)
		}
	}

	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if typ, ok := obj["type"].(string); ok && typ == "tool" {
		name, _ := obj["name"].(string)
		if name == "" {
			return nil, errors.New("tool_choice.tool requires name")
		}
		return map[string]any{"type": "tool", "name": name}, nil
	}

	return nil, errors.New("unsupported tool_choice object")
}

// mapThinkingToReasoning 将 Anthropic thinking 配置转换为 OpenAI reasoning 配置
func mapThinkingToReasoning(raw json.RawMessage) (*types.OpenAIReasoning, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("thinking must be an object: %w", err)
	}

	typeRaw, ok := obj["type"]
	if !ok {
		return nil, errors.New("thinking.type is required")
	}
	var thinkingType string
	if err := json.Unmarshal(typeRaw, &thinkingType); err != nil {
		return nil, fmt.Errorf("invalid thinking.type: %w", err)
	}

	switch thinkingType {
	case "disabled":
		return &types.OpenAIReasoning{Effort: "none"}, nil
	case "enabled":
		effort := "xhigh" // 默认拉满
		if budgetRaw, hasBudget := obj["budget_tokens"]; hasBudget {
			var budget int
			if err := json.Unmarshal(budgetRaw, &budget); err != nil {
				return nil, fmt.Errorf("invalid budget_tokens: %w", err)
			}
			switch {
			case budget <= 2048:
				effort = "low"
			case budget <= 8192:
				effort = "medium"
			case budget <= 32768:
				effort = "high"
			default:
				effort = "xhigh"
			}
		}
		return &types.OpenAIReasoning{Effort: effort, Summary: "auto"}, nil
	case "adaptive":
		// adaptive: 模型自行决定是否推理，映射为 high effort + auto summary
		effort := "high"
		if budgetRaw, hasBudget := obj["budget_tokens"]; hasBudget {
			var budget int
			if err := json.Unmarshal(budgetRaw, &budget); err == nil {
				switch {
				case budget <= 2048:
					effort = "low"
				case budget <= 8192:
					effort = "medium"
				case budget <= 32768:
					effort = "high"
				default:
					effort = "xhigh"
				}
			}
		}
		return &types.OpenAIReasoning{Effort: effort, Summary: "auto"}, nil
	default:
		return nil, fmt.Errorf("unsupported thinking.type: %s", thinkingType)
	}
}

// mapContextManagement 将 Anthropic 服务端 compaction beta 转换为 OpenAI context_management 格式
// Anthropic: {"edits": [{"type": "compact_20260112", "trigger_tokens": N}]}
// OpenAI:    [{"type": "compaction", "compact_threshold": N}]
func mapContextManagement(raw json.RawMessage) json.RawMessage {
	var anthCM struct {
		Edits []struct {
			Type          string `json:"type"`
			TriggerTokens int    `json:"trigger_tokens,omitempty"`
		} `json:"edits"`
	}
	if err := json.Unmarshal(raw, &anthCM); err != nil {
		log.Printf("WARN: failed to parse context_management: %v", err)
		return nil
	}

	var oaItems []map[string]any
	for _, edit := range anthCM.Edits {
		if !strings.HasPrefix(edit.Type, "compact") {
			continue
		}
		item := map[string]any{"type": "compaction"}
		if edit.TriggerTokens > 0 {
			item["compact_threshold"] = edit.TriggerTokens
		} else {
			item["compact_threshold"] = 160000 // 默认阈值
		}
		oaItems = append(oaItems, item)
	}

	if len(oaItems) == 0 {
		return nil
	}
	data, err := json.Marshal(oaItems)
	if err != nil {
		return nil
	}
	return data
}
