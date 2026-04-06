package types

import "encoding/json"

type AnthropicMessageRequest struct {
	Model             string                     `json:"model"`
	MaxTokens         int                        `json:"max_tokens"`
	Messages          []AnthropicMessage         `json:"messages"`
	System            json.RawMessage            `json:"system,omitempty"`
	Tools             []AnthropicTool            `json:"tools,omitempty"`
	ToolChoice        json.RawMessage            `json:"tool_choice,omitempty"`
	Stream            bool                       `json:"stream,omitempty"`
	Temperature       *float64                   `json:"temperature,omitempty"`
	TopP              *float64                   `json:"top_p,omitempty"`
	TopK              *int                       `json:"top_k,omitempty"`
	StopSequences     []string                   `json:"stop_sequences,omitempty"`
	Metadata          map[string]any             `json:"metadata,omitempty"`
	Thinking          json.RawMessage            `json:"thinking,omitempty"`
	Speed             string                     `json:"speed,omitempty"`              // "fast" = Claude Code /fast 模式
	ContextManagement json.RawMessage            `json:"context_management,omitempty"` // 服务端 compaction beta
	ExtraFields       map[string]json.RawMessage `json:"-"`
}

type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type AnthropicContentBlock struct {
	Type          string                     `json:"type"`
	Text          string                     `json:"text,omitempty"`
	Source        *AnthropicImage            `json:"source,omitempty"`
	ID            string                     `json:"id,omitempty"`
	Name          string                     `json:"name,omitempty"`
	Input         json.RawMessage            `json:"input,omitempty"`
	ToolUseID     string                     `json:"tool_use_id,omitempty"`
	Content       json.RawMessage            `json:"content,omitempty"`
	IsError       *bool                      `json:"is_error,omitempty"`
	Thinking      string                     `json:"thinking,omitempty"`
	Signature     string                     `json:"signature,omitempty"`
	Citations     json.RawMessage            `json:"citations,omitempty"`
	Status        string                     `json:"status,omitempty"`
	Execution     string                     `json:"execution,omitempty"`
	Action        json.RawMessage            `json:"action,omitempty"`
	Tools         json.RawMessage            `json:"tools,omitempty"`
	RevisedPrompt string                     `json:"revised_prompt,omitempty"`
	Result        string                     `json:"result,omitempty"`
	ToolName      string                     `json:"tool_name,omitempty"`
	Namespace     string                     `json:"namespace,omitempty"`
	Phase         string                     `json:"phase,omitempty"`
	EndTurn       *bool                      `json:"end_turn,omitempty"`
	ResponsesType string                     `json:"responses_type,omitempty"`
	RawInput      string                     `json:"raw_input,omitempty"`
	ExtraFields   map[string]json.RawMessage `json:"-"`
}

type AnthropicImage struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	FileID    string `json:"file_id,omitempty"`
}

type AnthropicTool struct {
	Type        string         `json:"type,omitempty"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
	MaxUses     int            `json:"max_uses,omitempty"` // server-side 工具（web_search）的调用次数限制
}

type AnthropicMessageResponse struct {
	ID                string                  `json:"id"`
	Type              string                  `json:"type"`
	Role              string                  `json:"role"`
	Content           []AnthropicContentBlock `json:"content"`
	Model             string                  `json:"model"`
	StopReason        *string                 `json:"stop_reason"`
	StopSequence      *string                 `json:"stop_sequence"`
	Usage             AnthropicUsage          `json:"usage"`
	ContextManagement json.RawMessage         `json:"context_management,omitempty"`
}

type AnthropicUsage struct {
	InputTokens              int                       `json:"input_tokens"`
	OutputTokens             int                       `json:"output_tokens"`
	CacheCreationInputTokens int                       `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                       `json:"cache_read_input_tokens"`
	ServerToolUse            *AnthropicServerToolUse   `json:"server_tool_use,omitempty"`
	ServiceTier              string                    `json:"service_tier,omitempty"`
	CacheCreation            *AnthropicCacheCreation   `json:"cache_creation,omitempty"`
	InferenceGeo             string                    `json:"inference_geo,omitempty"`
	Iterations               []AnthropicUsageIteration `json:"iterations,omitempty"`
	Speed                    string                    `json:"speed,omitempty"`
}

type AnthropicServerToolUse struct {
	WebSearchRequests int `json:"web_search_requests,omitempty"`
	WebFetchRequests  int `json:"web_fetch_requests,omitempty"`
}

type AnthropicCacheCreation struct {
	Ephemeral1HInputTokens int `json:"ephemeral_1h_input_tokens,omitempty"`
	Ephemeral5MInputTokens int `json:"ephemeral_5m_input_tokens,omitempty"`
}

type AnthropicUsageIteration struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type AnthropicError struct {
	Type  string             `json:"type"`
	Error AnthropicErrorBody `json:"error"`
}

type AnthropicErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type AnthropicStreamMessageStart struct {
	Type    string                   `json:"type"`
	Message AnthropicMessageResponse `json:"message"`
}

type AnthropicStreamContentBlockStart struct {
	Type         string                `json:"type"`
	Index        int                   `json:"index"`
	ContentBlock AnthropicContentBlock `json:"content_block"`
}

type AnthropicStreamContentBlockDelta struct {
	Type  string         `json:"type"`
	Index int            `json:"index"`
	Delta map[string]any `json:"delta"`
}

type AnthropicStreamContentBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type AnthropicStreamMessageDelta struct {
	Type              string                `json:"type"`
	Delta             AnthropicMessageDelta `json:"delta"`
	Usage             *AnthropicUsage       `json:"usage,omitempty"`
	ContextManagement json.RawMessage       `json:"context_management,omitempty"`
}

type AnthropicMessageDelta struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence,omitempty"`
}

type AnthropicStreamMessageStop struct {
	Type string `json:"type"`
}

func (b AnthropicContentBlock) MarshalJSON() ([]byte, error) {
	type alias AnthropicContentBlock
	base, err := json.Marshal(alias(b))
	if err != nil {
		return nil, err
	}
	if len(b.ExtraFields) == 0 {
		return base, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for key, val := range b.ExtraFields {
		if len(val) == 0 {
			continue
		}
		merged[key] = append(json.RawMessage(nil), val...)
	}
	return json.Marshal(merged)
}

func (b *AnthropicContentBlock) UnmarshalJSON(data []byte) error {
	type alias AnthropicContentBlock
	var parsed alias
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, key := range []string{
		"type",
		"text",
		"source",
		"id",
		"name",
		"input",
		"tool_use_id",
		"content",
		"is_error",
		"thinking",
		"signature",
		"citations",
		"status",
		"execution",
		"action",
		"tools",
		"revised_prompt",
		"result",
		"tool_name",
		"namespace",
		"phase",
		"end_turn",
		"responses_type",
		"raw_input",
	} {
		delete(raw, key)
	}
	*b = AnthropicContentBlock(parsed)
	if len(raw) > 0 {
		b.ExtraFields = raw
	}
	return nil
}
