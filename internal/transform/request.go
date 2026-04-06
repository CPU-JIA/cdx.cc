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

var openAIPassthroughFields = map[string]struct{}{
	"include":                {},
	"parallel_tool_calls":    {},
	"previous_response_id":   {},
	"prompt_cache_key":       {},
	"prompt_cache_retention": {},
	"service_tier":           {},
	"store":                  {},
	"text":                   {},
}

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

	if len(raw) > 0 {
		req.ExtraFields = make(map[string]json.RawMessage, len(raw))
		for key, val := range raw {
			req.ExtraFields[key] = val
		}
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
		Stream:          req.Stream,
		Store:           &storeTrue,
	}
	// 不把 Anthropic metadata 原样转发到上游。
	// 一些兼容代理会在携带 metadata 时卡住或返回 5xx。
	// 这些 metadata 仍可在 bridge 内部用于 prompt_cache_key 推导等本地逻辑。
	if os.Getenv("DISABLE_RESPONSE_STORAGE") != "" {
		storeFalse := false
		oa.Store = &storeFalse
	}
	if len(req.StopSequences) > 0 {
		oa.Stop = append([]string{}, req.StopSequences...)
	}

	if tier := mapSpeedToServiceTier(req.Speed); tier != "" {
		oa.ServiceTier = tier
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
	callKinds := make(map[string]string)
	for _, msg := range req.Messages {
		items, err := messageToInputItems(msg, mode, callKinds)
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

	if err := applyOpenAIPassthroughExtras(&oa, req.ExtraFields, mode); err != nil {
		return oa, err
	}

	if os.Getenv("DISABLE_RESPONSE_STORAGE") != "" {
		storeFalse := false
		oa.Store = &storeFalse
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

func messageToInputItems(msg types.AnthropicMessage, mode config.Mode, callKinds map[string]string) ([]types.OpenAIInputItem, error) {
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
			if current.Phase == "" && block.Phase != "" {
				current.Phase = block.Phase
			}
			if current.EndTurn == nil && block.EndTurn != nil {
				current.EndTurn = block.EndTurn
			}
			contentType := "input_text"
			if role == roleAssistant {
				contentType = "output_text"
			}
			content := types.OpenAIInputContent{Type: contentType, Text: block.Text}
			if role == roleAssistant && strings.TrimSpace(block.ResponsesType) == "refusal" {
				content.Type = "refusal"
				content.Text = ""
				content.Refusal = block.Text
			}
			current.Content = append(current.Content, content)
		case "image":
			if block.Source == nil {
				return nil, errors.New("image block missing source")
			}
			imageURL, fileID, err := imageSourceToInputImageFields(*block.Source)
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
				FileID:   fileID,
			})
		case "document":
			fileContent, err := documentBlockToInputContent(block)
			if err != nil {
				if mode == config.ModeStrict {
					return nil, err
				}
				log.Printf("WARN: converting document block to text placeholder: %v", err)
				docName := firstNonEmpty(block.Name, "unknown")
				if current == nil {
					current = &types.OpenAIInputItem{Type: "message", Role: role}
				}
				current.Content = append(current.Content, types.OpenAIInputContent{
					Type: "input_text",
					Text: fmt.Sprintf("[document: %s]", docName),
				})
				continue
			}
			if current == nil {
				current = &types.OpenAIInputItem{Type: "message", Role: role}
			}
			current.Content = append(current.Content, fileContent)
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
			item, kind, err := toolUseBlockToInputItem(block, mode)
			if err != nil {
				return nil, err
			}
			if callKinds != nil && kind != "" {
				callKinds[block.ID] = kind
			}
			items = append(items, item)
		case "server_tool_use":
			flush()
			if strings.TrimSpace(block.ID) == "" {
				return nil, errors.New("server_tool_use requires id")
			}
			serverItem, err := serverToolUseToInputItem(block, mode)
			if err != nil {
				return nil, err
			}
			if callKinds != nil {
				callKinds[block.ID] = serverItem.Type
			}
			items = append(items, serverItem)
		case "tool_result":
			flush()
			if block.ToolUseID == "" {
				return nil, errors.New("tool_result requires tool_use_id")
			}
			kind := ""
			if callKinds != nil {
				kind = callKinds[block.ToolUseID]
			}
			outputItem, err := toolResultToInputItem(block, mode, kind)
			if err != nil {
				return nil, err
			}
			if outputItem.Type != "" {
				items = append(items, outputItem)
			}
		case "web_search_tool_result":
			flush()
			// 历史中保留 server tool / tool search 结果块即可，不必重复构造成输入项。
			continue
		case "tool_search_tool_result":
			flush()
			item, err := toolSearchResultBlockToInputItem(block, mode)
			if err != nil {
				return nil, err
			}
			if item.Type != "" {
				items = append(items, item)
			}
			continue
		case "compaction":
			flush()
			compactionItem, err := compactionBlockToInputItem(block, mode)
			if err != nil {
				return nil, err
			}
			if compactionItem.Type != "" {
				items = append(items, compactionItem)
			}
		case "image_generation_call":
			flush()
			imageGenItem, err := imageGenerationBlockToInputItem(block, mode)
			if err != nil {
				return nil, err
			}
			if imageGenItem.Type != "" {
				items = append(items, imageGenItem)
			}
		case "responses_output_item":
			flush()
			opaqueItem, err := opaqueBlockToInputItem(block, mode)
			if err != nil {
				return nil, err
			}
			if opaqueItem.Type != "" {
				items = append(items, opaqueItem)
			}
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

func imageSourceToInputImageFields(src types.AnthropicImage) (string, string, error) {
	switch src.Type {
	case "base64":
		if src.MediaType == "" || src.Data == "" {
			return "", "", errors.New("base64 image requires media_type and data")
		}
		if _, err := base64.StdEncoding.DecodeString(src.Data); err != nil {
			return "", "", errors.New("invalid base64 image data")
		}
		return fmt.Sprintf("data:%s;base64,%s", src.MediaType, src.Data), "", nil
	case "url":
		if src.URL == "" {
			return "", "", errors.New("image url missing")
		}
		return src.URL, "", nil
	case "file_id":
		if strings.TrimSpace(src.FileID) == "" {
			return "", "", errors.New("image file_id missing")
		}
		return "", strings.TrimSpace(src.FileID), nil
	default:
		if strings.TrimSpace(src.FileID) != "" {
			return "", strings.TrimSpace(src.FileID), nil
		}
		return "", "", fmt.Errorf("unsupported image source type: %s", src.Type)
	}
}

func documentBlockToInputContent(block types.AnthropicContentBlock) (types.OpenAIInputContent, error) {
	if block.Source == nil {
		return types.OpenAIInputContent{}, errors.New("document block missing source")
	}
	return documentSourceToInputContent(*block.Source, block.Name)
}

func documentSourceToInputContent(src types.AnthropicImage, filename string) (types.OpenAIInputContent, error) {
	content := types.OpenAIInputContent{Type: "input_file"}
	switch src.Type {
	case "base64":
		if strings.TrimSpace(src.Data) == "" {
			return types.OpenAIInputContent{}, errors.New("base64 document requires data")
		}
		if _, err := base64.StdEncoding.DecodeString(src.Data); err != nil {
			return types.OpenAIInputContent{}, errors.New("invalid base64 document data")
		}
		content.FileData = src.Data
	case "url":
		if strings.TrimSpace(src.URL) == "" {
			return types.OpenAIInputContent{}, errors.New("document url missing")
		}
		content.FileURL = src.URL
	default:
		if strings.TrimSpace(src.FileID) != "" {
			content.FileID = src.FileID
			break
		}
		return types.OpenAIInputContent{}, fmt.Errorf("unsupported document source type: %s", src.Type)
	}
	if strings.TrimSpace(filename) != "" {
		content.Filename = filename
	} else if strings.TrimSpace(src.MediaType) == "application/pdf" {
		content.Filename = "document.pdf"
	}
	return content, nil
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
		var structured []map[string]any
		hasNonText := false
		for _, cb := range blocks {
			switch cb.Type {
			case "text":
				outParts = append(outParts, cb.Text)
				structured = append(structured, map[string]any{
					"type": "input_text",
					"text": cb.Text,
				})
			case "image":
				if cb.Source == nil {
					if mode == config.ModeStrict {
						return nil, errors.New("tool_result image block missing source")
					}
					continue
				}
				imageURL, fileID, err := imageSourceToInputImageFields(*cb.Source)
				if err != nil {
					if mode == config.ModeStrict {
						return nil, err
					}
					log.Printf("WARN: skipping tool_result image block: %v", err)
					continue
				}
				hasNonText = true
				part := map[string]any{
					"type": "input_image",
				}
				if imageURL != "" {
					part["image_url"] = imageURL
				}
				if fileID != "" {
					part["file_id"] = fileID
				}
				structured = append(structured, part)
			case "document":
				fileContent, err := documentBlockToInputContent(cb)
				if err != nil {
					if mode == config.ModeStrict {
						return nil, err
					}
					log.Printf("WARN: skipping tool_result document block: %v", err)
					continue
				}
				hasNonText = true
				doc := map[string]any{
					"type": "input_file",
				}
				if fileContent.FileData != "" {
					doc["file_data"] = fileContent.FileData
				}
				if fileContent.FileID != "" {
					doc["file_id"] = fileContent.FileID
				}
				if fileContent.FileURL != "" {
					doc["file_url"] = fileContent.FileURL
				}
				if fileContent.Filename != "" {
					doc["filename"] = fileContent.Filename
				}
				structured = append(structured, doc)
				outParts = append(outParts, documentBlockSummary(cb))
			case "search_result":
				text := searchResultBlockText(cb)
				if text == "" {
					if mode == config.ModeStrict {
						return nil, errors.New("search_result block must contain text-like fields")
					}
					continue
				}
				outParts = append(outParts, text)
				structured = append(structured, map[string]any{
					"type": "input_text",
					"text": text,
				})
			default:
				if mode == config.ModeStrict {
					return nil, fmt.Errorf("unsupported tool_result block: %s", cb.Type)
				}
			}
		}
		joined := strings.Join(outParts, "\n")
		if block.IsError != nil && *block.IsError && !hasNonText {
			joined = "ERROR: " + joined
		}
		if hasNonText {
			if block.IsError != nil && *block.IsError {
				if len(structured) > 0 {
					if structured[0]["type"] == "input_text" {
						if text, ok := structured[0]["text"].(string); ok {
							structured[0]["text"] = "ERROR: " + text
						}
					} else {
						structured = append([]map[string]any{{
							"type": "input_text",
							"text": "ERROR: tool execution failed",
						}}, structured...)
					}
				} else {
					structured = append(structured, map[string]any{
						"type": "input_text",
						"text": "ERROR: tool execution failed",
					})
				}
			}
			return structured, nil
		}
		return joined, nil
	}

	if mode == config.ModeStrict {
		return nil, errors.New("tool_result content must be string or array")
	}
	return string(bytes.TrimSpace(block.Content)), nil
}

func toolResultToComputerOutput(block types.AnthropicContentBlock, mode config.Mode) (any, map[string]json.RawMessage, error) {
	if restored, ok, err := storedResponseItemToInputItem(block, mode); ok || err != nil {
		if err != nil {
			return nil, nil, err
		}
		extra := cloneRawMap(restored.ExtraFields)
		return restored.Output, extra, nil
	}

	output, err := toolResultToOutput(block, mode)
	if err != nil {
		return nil, nil, err
	}

	switch typed := output.(type) {
	case []map[string]any:
		for _, item := range typed {
			if item["type"] != "input_image" {
				continue
			}
			screenshot := map[string]any{
				"type": "computer_screenshot",
			}
			if imageURL, _ := item["image_url"].(string); strings.TrimSpace(imageURL) != "" {
				screenshot["image_url"] = imageURL
			}
			if fileID, _ := item["file_id"].(string); strings.TrimSpace(fileID) != "" {
				screenshot["file_id"] = fileID
			}
			return screenshot, nil, nil
		}
	}

	return output, nil, nil
}

func documentBlockSummary(block types.AnthropicContentBlock) string {
	name := strings.TrimSpace(block.Name)
	if name == "" {
		name = "document"
	}
	return fmt.Sprintf("[document: %s]", name)
}

func searchResultBlockText(block types.AnthropicContentBlock) string {
	var parts []string
	if title := extraFieldString(block.ExtraFields, "title"); title != "" {
		parts = append(parts, title)
	}
	if url := extraFieldString(block.ExtraFields, "url"); url != "" {
		parts = append(parts, url)
	}
	if text := strings.TrimSpace(block.Text); text != "" {
		parts = append(parts, text)
	}
	for _, key := range []string{"text", "snippet", "excerpt", "description"} {
		if value := extraFieldString(block.ExtraFields, key); value != "" {
			parts = append(parts, value)
		}
	}
	if raw := extraFieldJSONText(block.ExtraFields, "content"); raw != "" {
		parts = append(parts, raw)
	}
	if raw := extraFieldJSONText(block.ExtraFields, "search_result"); raw != "" {
		parts = append(parts, raw)
	}
	if len(parts) == 0 && len(block.ExtraFields) > 0 {
		if raw, err := json.Marshal(block.ExtraFields); err == nil {
			parts = append(parts, string(raw))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func extraFieldString(extra map[string]json.RawMessage, key string) string {
	if len(extra) == 0 {
		return ""
	}
	raw, ok := extra[key]
	if !ok || len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

func extraFieldJSONText(extra map[string]json.RawMessage, key string) string {
	if len(extra) == 0 {
		return ""
	}
	raw, ok := extra[key]
	if !ok || len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(raw))
}

func extraFieldJSON(extra map[string]json.RawMessage, key string) json.RawMessage {
	if len(extra) == 0 {
		return nil
	}
	raw, ok := extra[key]
	if !ok || len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func toolResultToInputItem(block types.AnthropicContentBlock, mode config.Mode, callKind string) (types.OpenAIInputItem, error) {
	switch callKind {
	case "tool_search":
		tools, err := toolResultToToolSearchTools(block, mode)
		if err != nil {
			return types.OpenAIInputItem{}, err
		}
		return types.OpenAIInputItem{
			Type:      "tool_search_output",
			CallID:    block.ToolUseID,
			Status:    "completed",
			Execution: "client",
			Tools:     tools,
		}, nil
	case "custom_tool_call":
		output, err := toolResultToOutput(block, mode)
		if err != nil {
			return types.OpenAIInputItem{}, err
		}
		return types.OpenAIInputItem{
			Type:   "custom_tool_call_output",
			CallID: block.ToolUseID,
			Output: output,
		}, nil
	case "computer_call":
		output, extra, err := toolResultToComputerOutput(block, mode)
		if err != nil {
			return types.OpenAIInputItem{}, err
		}
		item := types.OpenAIInputItem{
			Type:   "computer_call_output",
			CallID: block.ToolUseID,
			Status: firstNonEmpty(block.Status, "completed"),
			Output: output,
		}
		if len(extra) > 0 {
			item.ExtraFields = extra
		}
		return item, nil
	case "file_search_call", "mcp_call":
		if len(block.ExtraFields) > 0 {
			if _, ok := block.ExtraFields[storedResponsesItemExtraKey]; ok {
				return types.OpenAIInputItem{}, nil
			}
		}
		return types.OpenAIInputItem{}, nil
	default:
		output, err := toolResultToOutput(block, mode)
		if err != nil {
			return types.OpenAIInputItem{}, err
		}
		return types.OpenAIInputItem{
			Type:   "function_call_output",
			CallID: block.ToolUseID,
			Output: output,
		}, nil
	}
}

func toolResultToToolSearchTools(block types.AnthropicContentBlock, mode config.Mode) ([]any, error) {
	if len(block.Content) == 0 {
		return []any{}, nil
	}

	if block.Content[0] != '[' {
		if mode == config.ModeStrict {
			return nil, errors.New("tool_search result must be an array of content blocks")
		}
		return []any{}, nil
	}

	var rawItems []map[string]any
	if err := json.Unmarshal(block.Content, &rawItems); err != nil {
		return nil, err
	}

	tools := make([]any, 0, len(rawItems))
	for _, item := range rawItems {
		typ, _ := item["type"].(string)
		switch typ {
		case "tool_reference":
			name, _ := item["tool_name"].(string)
			if name == "" {
				continue
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"name": name,
			})
		case "text":
			// 忽略 ToolSearch “Tool loaded.” 之类的人类可读占位文本。
			continue
		default:
			if mode == config.ModeStrict {
				return nil, fmt.Errorf("unsupported tool_search result block: %s", typ)
			}
		}
	}
	return tools, nil
}

func toolSearchResultBlockToInputItem(block types.AnthropicContentBlock, mode config.Mode) (types.OpenAIInputItem, error) {
	if restored, ok, err := storedResponseItemToInputItem(block, mode); ok || err != nil {
		return restored, err
	}
	if len(block.Tools) > 0 {
		var exact []any
		if err := json.Unmarshal(block.Tools, &exact); err == nil {
			return types.OpenAIInputItem{
				Type:      "tool_search_output",
				CallID:    block.ToolUseID,
				Status:    firstNonEmpty(block.Status, "completed"),
				Execution: firstNonEmpty(block.Execution, "server"),
				Tools:     exact,
			}, nil
		} else if mode == config.ModeStrict {
			return types.OpenAIInputItem{}, fmt.Errorf("invalid tool_search_tool_result tools: %w", err)
		}
	}
	tools, err := toolResultToToolSearchTools(types.AnthropicContentBlock{
		Content: block.Content,
	}, mode)
	if err != nil {
		return types.OpenAIInputItem{}, err
	}
	return types.OpenAIInputItem{
		Type:      "tool_search_output",
		CallID:    block.ToolUseID,
		Status:    firstNonEmpty(block.Status, "completed"),
		Execution: firstNonEmpty(block.Execution, "server"),
		Tools:     tools,
	}, nil
}

func classifyToolCallKind(name string) string {
	switch strings.TrimSpace(strings.ToLower(name)) {
	case "toolsearch", "tool_search", "toolsearchtool":
		return "tool_search"
	case "bash", "powershell":
		return "local_shell_call"
	case "filesearch", "file_search":
		return "file_search_call"
	case "computer", "computeruse":
		return "computer_call"
	case "mcp", "mcpcall":
		return "mcp_call"
	case "mcplisttools", "mcp_list_tools":
		return "mcp_list_tools"
	case "web_search":
		return "web_search_call"
	default:
		return "function_call"
	}
}

func toolUseBlockToInputItem(block types.AnthropicContentBlock, mode config.Mode) (types.OpenAIInputItem, string, error) {
	kind := strings.TrimSpace(block.ResponsesType)
	if kind == "" {
		kind = classifyToolCallKind(block.Name)
	}
	if restored, ok, err := storedResponseItemToInputItem(block, mode); ok || err != nil {
		return restored, normalizedToolCallKind(restored.Type, kind), err
	}
	args := strings.TrimSpace(string(block.Input))
	if args == "" {
		args = "{}"
	}

	switch kind {
	case "local_shell_call":
		action, err := localShellActionFromToolUse(block, mode)
		if err != nil {
			return types.OpenAIInputItem{}, "", err
		}
		return types.OpenAIInputItem{
			Type:   "local_shell_call",
			CallID: block.ID,
			Status: firstNonEmpty(block.Status, "completed"),
			Action: action,
		}, kind, nil
	case "tool_search":
		var parsed any
		if err := json.Unmarshal([]byte(args), &parsed); err != nil {
			if mode == config.ModeStrict {
				return types.OpenAIInputItem{}, "", fmt.Errorf("tool_search input must be valid JSON: %w", err)
			}
			parsed = map[string]any{"_raw": args}
		}
		return types.OpenAIInputItem{
			Type:      "tool_search_call",
			CallID:    block.ID,
			Status:    firstNonEmpty(block.Status, "completed"),
			Execution: firstNonEmpty(block.Execution, "client"),
			Arguments: mustMarshalRaw(parsed),
		}, kind, nil
	case "custom_tool_call":
		rawInput := block.RawInput
		if rawInput == "" {
			rawInput = args
		}
		return types.OpenAIInputItem{
			Type:      "custom_tool_call",
			CallID:    block.ID,
			Name:      block.Name,
			Namespace: block.Namespace,
			Input:     mustMarshalRaw(rawInput),
		}, kind, nil
	case "file_search_call", "computer_call", "mcp_call", "mcp_list_tools":
		item, err := specialToolUseBlockToInputItem(block, kind)
		return item, kind, err
	default:
		return types.OpenAIInputItem{
			Type:      "function_call",
			CallID:    block.ID,
			Name:      block.Name,
			Namespace: block.Namespace,
			Arguments: mustMarshalRaw(args),
		}, kind, nil
	}
}

func normalizedToolCallKind(itemType, fallback string) string {
	switch strings.TrimSpace(itemType) {
	case "tool_search_call":
		return "tool_search"
	case "":
		return fallback
	default:
		return itemType
	}
}

func storedResponseItemToInputItem(block types.AnthropicContentBlock, mode config.Mode) (types.OpenAIInputItem, bool, error) {
	if len(block.ExtraFields) == 0 {
		return types.OpenAIInputItem{}, false, nil
	}
	raw, ok := block.ExtraFields[storedResponsesItemExtraKey]
	if !ok || len(raw) == 0 {
		return types.OpenAIInputItem{}, false, nil
	}
	var item types.OpenAIOutputItem
	if err := json.Unmarshal(raw, &item); err != nil {
		if mode == config.ModeStrict {
			return types.OpenAIInputItem{}, true, fmt.Errorf("invalid stored responses_item: %w", err)
		}
		return types.OpenAIInputItem{}, false, nil
	}
	input, err := outputItemToInputItem(item)
	if err != nil {
		if mode == config.ModeStrict {
			return types.OpenAIInputItem{}, true, err
		}
		return types.OpenAIInputItem{}, false, nil
	}
	return input, true, nil
}

func specialToolUseBlockToInputItem(block types.AnthropicContentBlock, itemType string) (types.OpenAIInputItem, error) {
	item := types.OpenAIInputItem{
		Type:        itemType,
		Name:        block.Name,
		Namespace:   block.Namespace,
		Status:      block.Status,
		Execution:   block.Execution,
		Action:      cloneRaw(block.Action),
		ExtraFields: cloneRawMap(block.ExtraFields),
	}
	if item.ExtraFields != nil {
		delete(item.ExtraFields, storedResponsesItemExtraKey)
		if len(item.ExtraFields) == 0 {
			item.ExtraFields = nil
		}
	}
	if len(block.Tools) > 0 {
		var tools []any
		if err := json.Unmarshal(block.Tools, &tools); err == nil {
			item.Tools = tools
		} else {
			if item.ExtraFields == nil {
				item.ExtraFields = map[string]json.RawMessage{}
			}
			item.ExtraFields["tools"] = cloneRaw(block.Tools)
		}
	}

	switch itemType {
	case "mcp_list_tools":
		item.ID = block.ID
	default:
		item.CallID = block.ID
	}

	if strings.TrimSpace(block.RawInput) != "" {
		switch itemType {
		case "mcp_call":
			item.Arguments = mustMarshalRaw(block.RawInput)
		default:
			item.Input = mustMarshalRaw(block.RawInput)
		}
	} else if len(block.Input) > 0 {
		switch itemType {
		case "mcp_call":
			item.Arguments = cloneRaw(block.Input)
		default:
			item.Input = cloneRaw(block.Input)
		}
	}
	if len(block.Input) > 0 {
		applySpecialToolInputSemantics(&item, block.Input)
	}
	if raw := extraFieldJSON(block.ExtraFields, "error"); len(raw) > 0 {
		item.Error = raw
	}
	if raw := extraFieldJSON(block.ExtraFields, "output"); len(raw) > 0 {
		item.Output = cloneRaw(raw)
	}

	if item.Action == nil && len(block.Input) > 0 && (itemType == "file_search_call" || itemType == "computer_call" || itemType == "mcp_list_tools") {
		item.Action = cloneRaw(block.Input)
	}

	return item, nil
}

func applySpecialToolInputSemantics(item *types.OpenAIInputItem, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return
	}

	switch item.Type {
	case "file_search_call":
		if len(item.Queries) == 0 {
			var queries []string
			if qraw, ok := obj["queries"]; ok && json.Unmarshal(qraw, &queries) == nil {
				item.Queries = queries
			} else if qraw, ok := obj["query"]; ok {
				var query string
				if json.Unmarshal(qraw, &query) == nil && strings.TrimSpace(query) != "" {
					item.Queries = []string{query}
				}
			}
		}
		if len(item.Results) == 0 {
			if rraw, ok := obj["results"]; ok {
				item.Results = cloneRaw(rraw)
			}
		}
	case "computer_call", "mcp_call":
		if item.Output == nil {
			if oraw, ok := obj["output"]; ok {
				item.Output = cloneRaw(oraw)
			}
		}
		if len(item.Error) == 0 {
			if eraw, ok := obj["error"]; ok {
				item.Error = cloneRaw(eraw)
			}
		}
	}

	if item.ExtraFields == nil {
		item.ExtraFields = map[string]json.RawMessage{}
	}
	if _, ok := item.ExtraFields["server_label"]; !ok {
		if raw, ok := obj["server_label"]; ok {
			item.ExtraFields["server_label"] = cloneRaw(raw)
		}
	}
	if len(item.Tools) == 0 {
		if traw, ok := obj["tools"]; ok {
			var tools []any
			if json.Unmarshal(traw, &tools) == nil {
				item.Tools = tools
			}
		}
	}
	if len(item.ExtraFields) == 0 {
		item.ExtraFields = nil
	}
}

func localShellActionFromToolUse(block types.AnthropicContentBlock, mode config.Mode) (json.RawMessage, error) {
	if len(block.Action) > 0 {
		var parsed any
		if err := json.Unmarshal(block.Action, &parsed); err == nil {
			return append(json.RawMessage(nil), block.Action...), nil
		} else if mode == config.ModeStrict {
			return nil, fmt.Errorf("invalid local_shell action: %w", err)
		}
	}

	var input map[string]any
	if len(block.Input) > 0 {
		if err := json.Unmarshal(block.Input, &input); err != nil {
			if mode == config.ModeStrict {
				return nil, fmt.Errorf("invalid local_shell input: %w", err)
			}
			input = map[string]any{}
		}
	}
	command, _ := input["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" {
		command = ""
	}

	timeoutMS := normalizeShellTimeout(input["timeout"])
	workingDirectory := firstString(
		input["working_directory"],
		input["cwd"],
		input["workdir"],
	)
	envMap := normalizeStringMap(input["env"])
	user := firstString(input["user"])

	switch strings.TrimSpace(strings.ToLower(block.Name)) {
	case "powershell":
		return mustMarshalRaw(map[string]any{
			"type":              "exec",
			"command":           []string{"powershell", "-Command", command},
			"timeout_ms":        timeoutMS,
			"working_directory": emptyToNil(workingDirectory),
			"env":               emptyMapToNil(envMap),
			"user":              emptyToNil(user),
		}), nil
	default:
		return mustMarshalRaw(map[string]any{
			"type":              "exec",
			"command":           []string{"bash", "-lc", command},
			"timeout_ms":        timeoutMS,
			"working_directory": emptyToNil(workingDirectory),
			"env":               emptyMapToNil(envMap),
			"user":              emptyToNil(user),
		}), nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstString(values ...any) string {
	for _, value := range values {
		if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func normalizeShellTimeout(value any) any {
	switch v := value.(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return v
		}
	}
	return nil
}

func normalizeStringMap(value any) map[string]string {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string)
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func emptyToNil(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func emptyMapToNil(value map[string]string) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func serverToolUseToInputItem(block types.AnthropicContentBlock, mode config.Mode) (types.OpenAIInputItem, error) {
	if restored, ok, err := storedResponseItemToInputItem(block, mode); ok || err != nil {
		return restored, err
	}
	name := strings.TrimSpace(strings.ToLower(block.Name))
	switch name {
	case "web_search":
		var input struct {
			Query string `json:"query"`
		}
		if len(block.Input) > 0 {
			if err := json.Unmarshal(block.Input, &input); err != nil && mode == config.ModeStrict {
				return types.OpenAIInputItem{}, fmt.Errorf("invalid server_tool_use input: %w", err)
			}
		}
		action := mustMarshalRaw(map[string]string{
			"type":  "search",
			"query": input.Query,
		})
		return types.OpenAIInputItem{
			Type:   "web_search_call",
			ID:     block.ID,
			Status: "completed",
			Action: action,
		}, nil
	default:
		if mode == config.ModeStrict {
			return types.OpenAIInputItem{}, fmt.Errorf("unsupported server_tool_use: %s", block.Name)
		}
		return types.OpenAIInputItem{}, nil
	}
}

func compactionBlockToInputItem(block types.AnthropicContentBlock, mode config.Mode) (types.OpenAIInputItem, error) {
	if len(block.Content) == 0 {
		if mode == config.ModeStrict {
			return types.OpenAIInputItem{}, errors.New("compaction block missing content")
		}
		return types.OpenAIInputItem{}, nil
	}

	var encrypted string
	if err := json.Unmarshal(block.Content, &encrypted); err != nil {
		if mode == config.ModeStrict {
			return types.OpenAIInputItem{}, fmt.Errorf("invalid compaction block content: %w", err)
		}
		return types.OpenAIInputItem{}, nil
	}
	if strings.TrimSpace(encrypted) == "" {
		return types.OpenAIInputItem{}, nil
	}
	return types.OpenAIInputItem{
		Type:             "compaction",
		EncryptedContent: encrypted,
	}, nil
}

func imageGenerationBlockToInputItem(block types.AnthropicContentBlock, mode config.Mode) (types.OpenAIInputItem, error) {
	if strings.TrimSpace(block.ID) == "" {
		if mode == config.ModeStrict {
			return types.OpenAIInputItem{}, errors.New("image_generation_call missing id")
		}
		return types.OpenAIInputItem{}, nil
	}
	return types.OpenAIInputItem{
		Type:          "image_generation_call",
		ID:            block.ID,
		Status:        block.Status,
		RevisedPrompt: block.RevisedPrompt,
		Result:        block.Result,
	}, nil
}

func opaqueBlockToInputItem(block types.AnthropicContentBlock, mode config.Mode) (types.OpenAIInputItem, error) {
	if len(block.Content) == 0 {
		if mode == config.ModeStrict {
			return types.OpenAIInputItem{}, errors.New("responses_output_item missing content")
		}
		return types.OpenAIInputItem{}, nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(block.Content, &raw); err != nil {
		if mode == config.ModeStrict {
			return types.OpenAIInputItem{}, fmt.Errorf("invalid responses_output_item content: %w", err)
		}
		return types.OpenAIInputItem{}, nil
	}

	item := types.OpenAIInputItem{}
	typeName := strings.TrimSpace(block.ResponsesType)
	if typeName == "" {
		_ = json.Unmarshal(raw["type"], &typeName)
	}
	item.Type = typeName
	_ = json.Unmarshal(raw["id"], &item.ID)
	_ = json.Unmarshal(raw["role"], &item.Role)
	_ = json.Unmarshal(raw["call_id"], &item.CallID)
	_ = json.Unmarshal(raw["name"], &item.Name)
	_ = json.Unmarshal(raw["namespace"], &item.Namespace)
	_ = json.Unmarshal(raw["status"], &item.Status)
	_ = json.Unmarshal(raw["execution"], &item.Execution)
	_ = json.Unmarshal(raw["revised_prompt"], &item.RevisedPrompt)
	_ = json.Unmarshal(raw["result"], &item.Result)
	_ = json.Unmarshal(raw["phase"], &item.Phase)
	_ = json.Unmarshal(raw["end_turn"], &item.EndTurn)
	_ = json.Unmarshal(raw["queries"], &item.Queries)
	if val, ok := raw["content"]; ok {
		_ = json.Unmarshal(val, &item.Content)
	}
	if val, ok := raw["arguments"]; ok {
		item.Arguments = append(json.RawMessage(nil), val...)
	}
	if val, ok := raw["input"]; ok {
		item.Input = append(json.RawMessage(nil), val...)
	}
	if val, ok := raw["output"]; ok {
		item.Output = append(json.RawMessage(nil), val...)
	}
	if val, ok := raw["error"]; ok {
		item.Error = append(json.RawMessage(nil), val...)
	}
	if val, ok := raw["action"]; ok {
		item.Action = append(json.RawMessage(nil), val...)
	}
	if val, ok := raw["tools"]; ok {
		_ = json.Unmarshal(val, &item.Tools)
	}
	if val, ok := raw["results"]; ok {
		item.Results = append(json.RawMessage(nil), val...)
	}
	if val, ok := raw["encrypted_content"]; ok {
		_ = json.Unmarshal(val, &item.EncryptedContent)
	}

	for _, key := range []string{
		"type",
		"id",
		"role",
		"content",
		"call_id",
		"name",
		"namespace",
		"arguments",
		"input",
		"output",
		"error",
		"status",
		"execution",
		"action",
		"tools",
		"queries",
		"results",
		"encrypted_content",
		"revised_prompt",
		"result",
		"phase",
		"end_turn",
	} {
		delete(raw, key)
	}
	if len(raw) > 0 {
		item.ExtraFields = raw
	}
	return item, nil
}

func applyOpenAIPassthroughExtras(oa *types.OpenAIResponsesRequest, extras map[string]json.RawMessage, mode config.Mode) error {
	if len(extras) == 0 {
		return nil
	}

	for key, raw := range extras {
		switch key {
		case "include":
			var include []string
			if err := json.Unmarshal(raw, &include); err != nil {
				if ferr := invalidPassthroughFieldError(key, err, mode); ferr != nil {
					return ferr
				}
				continue
			}
			oa.Include = append([]string{}, include...)
		case "parallel_tool_calls":
			var enabled bool
			if err := json.Unmarshal(raw, &enabled); err != nil {
				if ferr := invalidPassthroughFieldError(key, err, mode); ferr != nil {
					return ferr
				}
				continue
			}
			oa.ParallelToolCalls = &enabled
		case "previous_response_id":
			var id string
			if err := json.Unmarshal(raw, &id); err != nil {
				if ferr := invalidPassthroughFieldError(key, err, mode); ferr != nil {
					return ferr
				}
				continue
			}
			oa.PreviousResponseID = strings.TrimSpace(id)
		case "prompt_cache_key":
			var keyVal string
			if err := json.Unmarshal(raw, &keyVal); err != nil {
				if ferr := invalidPassthroughFieldError(key, err, mode); ferr != nil {
					return ferr
				}
				continue
			}
			oa.PromptCacheKey = strings.TrimSpace(keyVal)
		case "prompt_cache_retention":
			var retention string
			if err := json.Unmarshal(raw, &retention); err != nil {
				if ferr := invalidPassthroughFieldError(key, err, mode); ferr != nil {
					return ferr
				}
				continue
			}
			oa.PromptCacheRetention = strings.TrimSpace(retention)
		case "service_tier":
			var tier string
			if err := json.Unmarshal(raw, &tier); err != nil {
				if ferr := invalidPassthroughFieldError(key, err, mode); ferr != nil {
					return ferr
				}
				continue
			}
			oa.ServiceTier = strings.TrimSpace(tier)
		case "store":
			if os.Getenv("DISABLE_RESPONSE_STORAGE") != "" {
				continue
			}
			var store bool
			if err := json.Unmarshal(raw, &store); err != nil {
				if ferr := invalidPassthroughFieldError(key, err, mode); ferr != nil {
					return ferr
				}
				continue
			}
			oa.Store = &store
		case "text":
			var parsed any
			if err := json.Unmarshal(raw, &parsed); err != nil {
				if ferr := invalidPassthroughFieldError(key, err, mode); ferr != nil {
					return ferr
				}
				continue
			}
			oa.Text = append(json.RawMessage(nil), raw...)
		default:
			if oa.ExtraFields == nil {
				oa.ExtraFields = make(map[string]json.RawMessage)
			}
			oa.ExtraFields[key] = append(json.RawMessage(nil), raw...)
		}
	}

	return nil
}

func invalidPassthroughFieldError(key string, err error, mode config.Mode) error {
	if mode == config.ModeStrict {
		return fmt.Errorf("invalid %s: %w", key, err)
	}
	log.Printf("WARN: skipping invalid passthrough field %s: %v", key, err)
	return nil
}

func mapSpeedToServiceTier(speed string) string {
	switch strings.ToLower(strings.TrimSpace(speed)) {
	case "":
		return ""
	case "fast":
		return "priority"
	case "priority", "flex", "auto":
		return strings.ToLower(strings.TrimSpace(speed))
	default:
		return ""
	}
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
