package tokenizer

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"cdx.cc/claude-bridge/internal/types"

	tiktoken "github.com/tiktoken-go/tokenizer"
)

// 默认编码：o200k_base 用于 GPT-5.x/Codex，cl100k_base 用于 GPT-4
var (
	defaultCodec tiktoken.Codec
	initOnce     sync.Once
)

func getCodec() tiktoken.Codec {
	initOnce.Do(func() {
		enc, err := tiktoken.Get(tiktoken.O200kBase)
		if err != nil {
			log.Printf("WARN: failed to load o200k_base, falling back to cl100k_base: %v", err)
			enc, err = tiktoken.Get(tiktoken.Cl100kBase)
			if err != nil {
				log.Printf("ERROR: failed to load any tiktoken encoding: %v", err)
				return
			}
		}
		defaultCodec = enc
	})
	return defaultCodec
}

// CountText 对纯文本做 token 计数
func CountText(text string) int {
	codec := getCodec()
	if codec == nil {
		return estimateFallback(text)
	}
	ids, _, _ := codec.Encode(text)
	return len(ids)
}

// CountRequestBody 解析 Anthropic Messages API 请求体，精确计算 input token 数
// 提取 system prompt + messages 中的纯文本内容，跳过 JSON 结构开销
func CountRequestBody(body []byte) int {
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}

	if err := json.Unmarshal(body, &req); err != nil {
		// 解析失败 → 回退到粗估
		return estimateFallback(string(body))
	}

	var (
		textParts   []string
		extraTokens int
	)

	// system prompt
	if len(req.System) > 0 {
		sys := extractSystemText(req.System)
		if sys != "" {
			textParts = append(textParts, sys)
		}
	}

	// messages
	for _, msg := range req.Messages {
		parts, extra := extractContentFeatures(msg.Content)
		textParts = append(textParts, parts...)
		extraTokens += extra
	}

	// tools（tool 名称和描述也占 token）
	for _, tool := range req.Tools {
		if tool.Name != "" {
			textParts = append(textParts, tool.Name)
		}
		if tool.Description != "" {
			textParts = append(textParts, tool.Description)
		}
	}

	allText := strings.Join(textParts, "\n")
	if allText == "" {
		if extraTokens > 0 {
			return extraTokens
		}
		return estimateFallback(string(body))
	}

	count := CountText(allText)

	// JSON 结构开销补偿：role/type/key 名等约占 15%
	overhead := count * 15 / 100
	return count + overhead + extraTokens
}

func CountOpenAIResponsesRequest(req types.OpenAIResponsesRequest) int {
	var (
		textParts   []string
		extraTokens int
	)

	if instructions := strings.TrimSpace(req.Instructions); instructions != "" {
		textParts = append(textParts, instructions)
	}
	for _, item := range req.Input {
		parts, extra := extractOpenAIInputItemFeatures(item)
		textParts = append(textParts, parts...)
		extraTokens += extra
	}
	for _, tool := range req.Tools {
		if tool.Name != "" {
			textParts = append(textParts, tool.Name)
		}
		if tool.Description != "" {
			textParts = append(textParts, tool.Description)
		}
		if len(tool.Parameters) > 0 {
			if raw, err := json.Marshal(tool.Parameters); err == nil {
				textParts = append(textParts, string(raw))
			}
		}
	}

	allText := strings.Join(textParts, "\n")
	if allText == "" {
		if extraTokens > 0 {
			return extraTokens
		}
		return 1
	}

	count := CountText(allText)
	overhead := count * 15 / 100
	return count + overhead + extraTokens
}

// extractSystemText 从 system 字段提取文本（支持字符串和数组格式）
func extractSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// extractContentText 从 content 字段提取所有文本片段
func extractContentFeatures(raw json.RawMessage) ([]string, int) {
	if len(raw) == 0 {
		return nil, 0
	}
	// 纯字符串
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return []string{s}, 0
		}
	}
	// content block 数组
	if raw[0] == '[' {
		var blocks []types.AnthropicContentBlock
		if err := json.Unmarshal(raw, &blocks); err == nil {
			var (
				parts []string
				extra int
			)
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if b.Text != "" {
						parts = append(parts, b.Text)
					}
				case "thinking":
					if b.Thinking != "" {
						parts = append(parts, b.Thinking)
					}
				case "tool_use":
					if b.Name != "" {
						parts = append(parts, b.Name)
					}
					if len(b.Input) > 0 {
						parts = append(parts, string(b.Input))
					}
					if len(b.Action) > 0 {
						parts = append(parts, string(b.Action))
					}
					if len(b.Tools) > 0 {
						parts = append(parts, string(b.Tools))
					}
					if b.RawInput != "" {
						parts = append(parts, b.RawInput)
					}
					if len(b.ExtraFields) > 0 {
						if raw, err := json.Marshal(b.ExtraFields); err == nil {
							parts = append(parts, string(raw))
						}
					}
				case "server_tool_use":
					if b.Name != "" {
						parts = append(parts, b.Name)
					}
					if len(b.Input) > 0 {
						parts = append(parts, string(b.Input))
					}
					if len(b.ExtraFields) > 0 {
						if raw, err := json.Marshal(b.ExtraFields); err == nil {
							parts = append(parts, string(raw))
						}
					}
				case "image", "document":
					extra += 2000
					if b.Name != "" {
						parts = append(parts, b.Name)
					}
				case "tool_result":
					nestedParts, nestedExtra := extractContentFeatures(b.Content)
					parts = append(parts, nestedParts...)
					extra += nestedExtra
				case "web_search_tool_result", "tool_search_tool_result":
					nestedParts, nestedExtra := extractContentFeatures(b.Content)
					parts = append(parts, nestedParts...)
					extra += nestedExtra
				case "search_result":
					if text := tokenizerSearchResultText(b); text != "" {
						parts = append(parts, text)
					}
				case "compaction":
					if len(b.Content) > 0 {
						parts = append(parts, string(b.Content))
					}
				case "image_generation_call":
					if b.RevisedPrompt != "" {
						parts = append(parts, b.RevisedPrompt)
					}
					if b.Result != "" {
						parts = append(parts, b.Result)
					}
				case "responses_output_item":
					if len(b.Content) > 0 {
						parts = append(parts, string(b.Content))
					}
				case "tool_reference":
					if b.ToolName != "" {
						parts = append(parts, b.ToolName)
					}
				}
			}
			return parts, extra
		}
	}
	return nil, 0
}

func tokenizerSearchResultText(block types.AnthropicContentBlock) string {
	var parts []string
	for _, key := range []string{"title", "url"} {
		if value := tokenizerExtraFieldString(block.ExtraFields, key); value != "" {
			parts = append(parts, value)
		}
	}
	if text := strings.TrimSpace(block.Text); text != "" {
		parts = append(parts, text)
	}
	for _, key := range []string{"text", "snippet", "excerpt", "description"} {
		if value := tokenizerExtraFieldString(block.ExtraFields, key); value != "" {
			parts = append(parts, value)
		}
	}
	if len(parts) == 0 && len(block.ExtraFields) > 0 {
		if raw, err := json.Marshal(block.ExtraFields); err == nil {
			parts = append(parts, string(raw))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func tokenizerExtraFieldString(extra map[string]json.RawMessage, key string) string {
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

func extractOpenAIInputItemFeatures(item types.OpenAIInputItem) ([]string, int) {
	var (
		parts []string
		extra int
	)

	if strings.TrimSpace(item.Name) != "" {
		parts = append(parts, item.Name)
	}
	if strings.TrimSpace(item.Namespace) != "" {
		parts = append(parts, item.Namespace)
	}
	if strings.TrimSpace(item.EncryptedContent) != "" {
		parts = append(parts, item.EncryptedContent)
	}
	if strings.TrimSpace(item.RevisedPrompt) != "" {
		parts = append(parts, item.RevisedPrompt)
	}
	if strings.TrimSpace(item.Result) != "" {
		parts = append(parts, item.Result)
	}
	if len(item.Arguments) > 0 {
		parts = append(parts, strings.TrimSpace(string(item.Arguments)))
	}
	if len(item.Input) > 0 {
		parts = append(parts, strings.TrimSpace(string(item.Input)))
	}
	if len(item.Action) > 0 {
		parts = append(parts, strings.TrimSpace(string(item.Action)))
	}
	if len(item.Error) > 0 {
		parts = append(parts, strings.TrimSpace(string(item.Error)))
	}
	if len(item.Tools) > 0 {
		if raw, err := json.Marshal(item.Tools); err == nil {
			parts = append(parts, string(raw))
		}
	}
	if len(item.Queries) > 0 {
		parts = append(parts, item.Queries...)
	}
	if len(item.Results) > 0 {
		parts = append(parts, strings.TrimSpace(string(item.Results)))
	}
	if len(item.ExtraFields) > 0 {
		if raw, err := json.Marshal(item.ExtraFields); err == nil {
			parts = append(parts, string(raw))
		}
	}
	for _, content := range item.Content {
		switch content.Type {
		case "input_text", "output_text":
			if content.Text != "" {
				parts = append(parts, content.Text)
			}
		case "refusal":
			if content.Refusal != "" {
				parts = append(parts, content.Refusal)
			}
		case "input_image":
			extra += 2000
		case "input_file":
			extra += 2000
			if content.Filename != "" {
				parts = append(parts, content.Filename)
			}
		default:
			if content.Text != "" {
				parts = append(parts, content.Text)
			}
		}
	}

	switch output := item.Output.(type) {
	case string:
		parts = append(parts, output)
	case []map[string]any:
		parts2, extra2 := extractFunctionOutputMaps(output)
		parts = append(parts, parts2...)
		extra += extra2
	case []any:
		parts2, extra2 := extractFunctionOutputAny(output)
		parts = append(parts, parts2...)
		extra += extra2
	default:
		if output != nil {
			if raw, err := json.Marshal(output); err == nil {
				parts = append(parts, string(raw))
			}
		}
	}

	return parts, extra
}

func extractFunctionOutputMaps(items []map[string]any) ([]string, int) {
	var (
		parts []string
		extra int
	)
	for _, item := range items {
		typ, _ := item["type"].(string)
		switch typ {
		case "input_text":
			if text, _ := item["text"].(string); text != "" {
				parts = append(parts, text)
			}
		case "input_image", "input_file":
			extra += 2000
			if filename, _ := item["filename"].(string); filename != "" {
				parts = append(parts, filename)
			}
		default:
			if raw, err := json.Marshal(item); err == nil {
				parts = append(parts, string(raw))
			}
		}
	}
	return parts, extra
}

func extractFunctionOutputAny(items []any) ([]string, int) {
	var maps []map[string]any
	for _, item := range items {
		asMap, ok := item.(map[string]any)
		if !ok {
			return []string{fmt.Sprint(items...)}, 0
		}
		maps = append(maps, asMap)
	}
	return extractFunctionOutputMaps(maps)
}

// estimateFallback 在 tiktoken 不可用时的粗估回退
// 比原来的 len/3 更精确：先去 JSON 开销，再按 4 字符/token 估算
func estimateFallback(text string) int {
	if len(text) == 0 {
		return 1
	}
	// 中文字符占比检测：中文约 1.5 字符/token，英文约 4 字符/token
	chineseCount := 0
	totalChars := 0
	for _, r := range text {
		totalChars++
		if r >= '\u4e00' && r <= '\u9fff' {
			chineseCount++
		}
	}
	chineseRatio := float64(chineseCount) / float64(totalChars)
	// 加权平均：中文 1.5, 英文 4
	charsPerToken := 4.0 - 2.5*chineseRatio
	estimated := int(float64(totalChars) / charsPerToken)
	if estimated < 1 {
		return 1
	}
	return estimated
}
