package tokenizer

import (
	"encoding/json"
	"log"
	"strings"
	"sync"

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

	var textParts []string

	// system prompt
	if len(req.System) > 0 {
		sys := extractSystemText(req.System)
		if sys != "" {
			textParts = append(textParts, sys)
		}
	}

	// messages
	for _, msg := range req.Messages {
		textParts = append(textParts, extractContentText(msg.Content)...)
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
		return estimateFallback(string(body))
	}

	count := CountText(allText)

	// JSON 结构开销补偿：role/type/key 名等约占 15%
	overhead := count * 15 / 100
	return count + overhead
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
func extractContentText(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	// 纯字符串
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return []string{s}
		}
	}
	// content block 数组
	if raw[0] == '[' {
		var blocks []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			Content   json.RawMessage `json:"content"`
			ToolUseID string          `json:"tool_use_id"`
		}
		if err := json.Unmarshal(raw, &blocks); err == nil {
			var parts []string
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
				case "tool_result":
					parts = append(parts, extractContentText(b.Content)...)
				}
			}
			return parts
		}
	}
	return nil
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
