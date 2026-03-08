package transform

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"cdx.cc/claude-bridge/internal/config"
	"cdx.cc/claude-bridge/internal/types"
)

func TransformOpenAIToAnthropic(resp types.OpenAIResponse, mode config.Mode, requestModel string) (types.AnthropicMessageResponse, error) {
	content := make([]types.AnthropicContentBlock, 0, len(resp.Output))
	// thinking 块收集（放在所有 content 之前）
	var thinkingBlocks []types.AnthropicContentBlock
	toolUsed := false

	// 预收集所有 url_citation 注解（用于 web_search_tool_result）
	allAnnotations := collectAnnotations(resp.Output)

	for _, item := range resp.Output {
		switch item.Type {
		case "reasoning":
			// reasoning → thinking 块
			block, err := openAIReasoningToThinkingBlock(item)
			if err != nil {
				if mode == config.ModeStrict {
					return types.AnthropicMessageResponse{}, err
				}
				log.Printf("WARN: skipping reasoning block: %v", err)
				continue
			}
			if block != nil {
				thinkingBlocks = append(thinkingBlocks, *block)
			}
		case "message":
			blocks, err := openAIMessageToBlocks(item, mode)
			if err != nil {
				return types.AnthropicMessageResponse{}, err
			}
			content = append(content, blocks...)
		case "function_call":
			block, err := openAIFunctionCallToBlock(item, mode)
			if err != nil {
				return types.AnthropicMessageResponse{}, err
			}
			content = append(content, block)
			toolUsed = true
		case "custom_tool_call":
			block, err := openAICustomToolCallToBlock(item, mode)
			if err != nil {
				return types.AnthropicMessageResponse{}, err
			}
			content = append(content, block)
			toolUsed = true
		case "web_search_call":
			blocks := openAIWebSearchToBlocks(item, allAnnotations)
			content = append(content, blocks...)
		case "compaction":
			// 服务端 compaction 结果 → 静默跳过，不透传给客户端
			log.Printf("INFO: skipping compaction output item from upstream")
			continue
		default:
			if mode == config.ModeStrict {
				return types.AnthropicMessageResponse{}, fmt.Errorf("unsupported output item type: %s", item.Type)
			}
			log.Printf("WARN: converting unsupported output type to text: %s", item.Type)
			content = append(content, types.AnthropicContentBlock{
				Type: "text",
				Text: fmt.Sprintf("[unsupported output item: %s]", item.Type),
			})
		}
	}

	// thinking 块放在 content 最前面（Anthropic 规范）
	if len(thinkingBlocks) > 0 {
		content = append(thinkingBlocks, content...)
	}

	stopReason := deriveStopReason(resp, toolUsed)

	usage := types.AnthropicUsage{}
	if resp.Usage != nil {
		usage.InputTokens = resp.Usage.InputTokens
		usage.OutputTokens = resp.Usage.OutputTokens
	}

	return types.AnthropicMessageResponse{
		ID:         "msg_" + resp.ID,
		Type:       "message",
		Role:       "assistant",
		Content:    content,
		Model:      requestModel,
		StopReason: stopReason,
		Usage:      usage,
	}, nil
}

func openAIMessageToBlocks(item types.OpenAIOutputItem, mode config.Mode) ([]types.AnthropicContentBlock, error) {
	if len(item.Content) == 0 {
		return nil, nil
	}

	var parts []types.OpenAIMessageContent
	if err := json.Unmarshal(item.Content, &parts); err != nil {
		return nil, err
	}
	blocks := make([]types.AnthropicContentBlock, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "output_text":
			block := types.AnthropicContentBlock{Type: "text", Text: part.Text}
			if len(part.Annotations) > 0 {
				block.Citations = annotationsToCitations(part.Annotations, part.Text)
			}
			blocks = append(blocks, block)
		case "refusal":
			if mode == config.ModeStrict {
				return nil, errors.New("refusal is not supported in strict mode")
			}
			text := strings.TrimSpace(part.Refusal)
			if text == "" {
				text = "[refusal]"
			}
			blocks = append(blocks, types.AnthropicContentBlock{Type: "text", Text: text})
		default:
			if mode == config.ModeStrict {
				return nil, fmt.Errorf("unsupported message content type: %s", part.Type)
			}
		}
	}
	return blocks, nil
}

func openAIFunctionCallToBlock(item types.OpenAIOutputItem, mode config.Mode) (types.AnthropicContentBlock, error) {
	if item.CallID == "" || item.Name == "" {
		return types.AnthropicContentBlock{}, errors.New("function_call missing call_id or name")
	}

	input := map[string]any{}
	if item.Arguments != "" {
		if err := json.Unmarshal([]byte(item.Arguments), &input); err != nil {
			if mode == config.ModeStrict {
				return types.AnthropicContentBlock{}, err
			}
			input = map[string]any{"_raw": item.Arguments}
		}
	}

	return types.AnthropicContentBlock{
		Type:  "tool_use",
		ID:    item.CallID,
		Name:  item.Name,
		Input: mustMarshalRaw(input),
	}, nil
}

func openAICustomToolCallToBlock(item types.OpenAIOutputItem, mode config.Mode) (types.AnthropicContentBlock, error) {
	if item.CallID == "" || item.Name == "" {
		return types.AnthropicContentBlock{}, errors.New("custom_tool_call missing call_id or name")
	}

	input := map[string]any{}
	if len(item.Input) > 0 {
		if err := json.Unmarshal(item.Input, &input); err != nil {
			if mode == config.ModeStrict {
				return types.AnthropicContentBlock{}, err
			}
			input = map[string]any{"_raw": string(item.Input)}
		}
	}

	return types.AnthropicContentBlock{
		Type:  "tool_use",
		ID:    item.CallID,
		Name:  item.Name,
		Input: mustMarshalRaw(input),
	}, nil
}

// placeholderSignature 占位签名，让 Claude Code CLI 将 thinking 渲染为折叠 UI
// 我们的代理在下一轮请求时会剥离 thinking/signature 块，所以不会被验证
const placeholderSignature = "proxy-bridge-signature-placeholder"

// openAIReasoningToThinkingBlock 将 OpenAI reasoning 输出项转换为 Anthropic thinking 块
func openAIReasoningToThinkingBlock(item types.OpenAIOutputItem) (*types.AnthropicContentBlock, error) {
	// 优先从 summary 字段提取（reasoning item 的标准结构）
	if len(item.Summary) > 0 {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(item.Summary, &parts); err == nil && len(parts) > 0 {
			var sb strings.Builder
			for _, p := range parts {
				if p.Text != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(p.Text)
				}
			}
			if sb.Len() > 0 {
				return &types.AnthropicContentBlock{
					Type:      "thinking",
					Thinking:  sb.String(),
					Signature: placeholderSignature,
				}, nil
			}
		}
	}

	// fallback: 从 content 字段提取（reasoning_text 格式）
	if len(item.Content) > 0 {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(item.Content, &parts); err != nil {
			// 尝试纯字符串格式
			var text string
			if err2 := json.Unmarshal(item.Content, &text); err2 != nil {
				return nil, fmt.Errorf("failed to parse reasoning content: %w", err)
			}
			if text == "" {
				return nil, nil
			}
			return &types.AnthropicContentBlock{
				Type:      "thinking",
				Thinking:  text,
				Signature: placeholderSignature,
			}, nil
		}

		var sb strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(p.Text)
			}
		}
		if sb.Len() > 0 {
			return &types.AnthropicContentBlock{
				Type:      "thinking",
				Thinking:  sb.String(),
				Signature: placeholderSignature,
			}, nil
		}
	}

	return nil, nil
}

func mustMarshalRaw(val any) json.RawMessage {
	raw, _ := json.Marshal(val)
	return raw
}

func deriveStopReason(resp types.OpenAIResponse, toolUsed bool) *string {
	if resp.IncompleteDetails != nil && resp.IncompleteDetails.Reason != "" {
		reason := resp.IncompleteDetails.Reason
		switch reason {
		case "max_output_tokens":
			mapped := "max_tokens"
			return &mapped
		case "content_filter":
			mapped := "end_turn"
			return &mapped
		}
	}
	switch resp.Status {
	case "incomplete":
		mapped := "max_tokens"
		return &mapped
	case "failed", "cancelled":
		mapped := "end_turn"
		return &mapped
	}
	if toolUsed {
		mapped := "tool_use"
		return &mapped
	}
	mapped := "end_turn"
	return &mapped
}

// collectAnnotations 从所有 message 输出项中收集 url_citation 注解
func collectAnnotations(output []types.OpenAIOutputItem) []types.OpenAIAnnotation {
	var all []types.OpenAIAnnotation
	for _, item := range output {
		if item.Type != "message" || len(item.Content) == 0 {
			continue
		}
		var parts []types.OpenAIMessageContent
		if err := json.Unmarshal(item.Content, &parts); err != nil {
			continue
		}
		for _, part := range parts {
			for _, ann := range part.Annotations {
				if ann.Type == "url_citation" {
					all = append(all, ann)
				}
			}
		}
	}
	return all
}

// openAIWebSearchToBlocks 将 OpenAI web_search_call 转换为 Anthropic server_tool_use + web_search_tool_result
func openAIWebSearchToBlocks(item types.OpenAIOutputItem, annotations []types.OpenAIAnnotation) []types.AnthropicContentBlock {
	var action struct {
		Query string `json:"query"`
	}
	if len(item.Action) > 0 {
		_ = json.Unmarshal(item.Action, &action)
	}

	toolUseID := "srvtoolu_" + item.ID

	// server_tool_use 块
	serverToolUse := types.AnthropicContentBlock{
		Type:  "server_tool_use",
		ID:    toolUseID,
		Name:  "web_search",
		Input: mustMarshalRaw(map[string]string{"query": action.Query}),
	}

	// web_search_tool_result 块（从注解构建搜索结果）
	searchResults := annotationsToSearchResults(annotations)
	resultBlock := types.AnthropicContentBlock{
		Type:      "web_search_tool_result",
		ToolUseID: toolUseID,
		Content:   mustMarshalRaw(searchResults),
	}

	return []types.AnthropicContentBlock{serverToolUse, resultBlock}
}

// annotationsToSearchResults 将 url_citation 注解转为 web_search_result 数组
func annotationsToSearchResults(annotations []types.OpenAIAnnotation) []map[string]any {
	seen := make(map[string]bool)
	var results []map[string]any
	for _, ann := range annotations {
		if ann.Type != "url_citation" || seen[ann.URL] {
			continue
		}
		seen[ann.URL] = true
		results = append(results, map[string]any{
			"type":              "web_search_result",
			"url":               ann.URL,
			"title":             ann.Title,
			"encrypted_content": "<encrypted>",
		})
	}
	if results == nil {
		results = []map[string]any{}
	}
	return results
}

// annotationsToCitations 将 url_citation 注解转为 Anthropic citations 数组
// text 参数用于提取 cited_text（Anthropic 规范要求最多 150 字符的引用文本）
func annotationsToCitations(annotations []types.OpenAIAnnotation, text string) json.RawMessage {
	var citations []map[string]any
	for _, ann := range annotations {
		if ann.Type != "url_citation" {
			continue
		}
		citedText := extractCitedText(text, ann.StartIndex, ann.EndIndex)
		citations = append(citations, map[string]any{
			"type":             "web_search_result_location",
			"url":              ann.URL,
			"title":            ann.Title,
			"cited_text":       citedText,
			"encrypted_index":  "<encrypted>",
			"start_char_index": ann.StartIndex,
			"end_char_index":   ann.EndIndex,
		})
	}
	if len(citations) == 0 {
		return nil
	}
	return mustMarshalRaw(citations)
}

// extractCitedText 从原文中按索引截取引用文本，最多 150 字符
func extractCitedText(text string, start, end int) string {
	if start < 0 || end <= start || start >= len(text) {
		return ""
	}
	if end > len(text) {
		end = len(text)
	}
	cited := text[start:end]
	if len(cited) > 150 {
		cited = cited[:150]
	}
	return cited
}
