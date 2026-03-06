package types

import "encoding/json"

type OpenAIReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type OpenAIResponsesRequest struct {
	Model              string            `json:"model"`
	Input              []OpenAIInputItem `json:"input,omitempty"`
	Instructions       string            `json:"instructions,omitempty"`
	Tools              []OpenAITool      `json:"tools,omitempty"`
	ToolChoice         any               `json:"tool_choice,omitempty"`
	Stream             bool              `json:"stream,omitempty"`
	MaxOutputTokens    *int              `json:"max_output_tokens,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	TopP               *float64          `json:"top_p,omitempty"`
	Stop               []string          `json:"stop,omitempty"`
	Metadata           map[string]any    `json:"metadata,omitempty"`
	Store              *bool             `json:"store,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	Reasoning          *OpenAIReasoning  `json:"reasoning,omitempty"`
}

type OpenAIInputItem struct {
	Type      string               `json:"type,omitempty"`
	Role      string               `json:"role,omitempty"`
	Content   []OpenAIInputContent `json:"content,omitempty"`
	CallID    string               `json:"call_id,omitempty"`
	Name      string               `json:"name,omitempty"`
	Arguments string               `json:"arguments,omitempty"`
	Output    any                  `json:"output,omitempty"`
}

type OpenAIInputContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"` // Responses API: 平铺字符串，非嵌套对象
	Detail   string `json:"detail,omitempty"`    // input_image 的细节级别
	FileID   string `json:"file_id,omitempty"`
}

type OpenAITool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type OpenAIResponse struct {
	ID                string             `json:"id"`
	Model             string             `json:"model"`
	Output            []OpenAIOutputItem `json:"output"`
	Usage             *OpenAIUsage       `json:"usage,omitempty"`
	IncompleteDetails *OpenAIIncomplete  `json:"incomplete_details,omitempty"`
	Status            string             `json:"status,omitempty"`
}

type OpenAIUsage struct {
	InputTokens        int                    `json:"input_tokens"`
	OutputTokens       int                    `json:"output_tokens"`
	OutputTokenDetails *OpenAIOutputTokenInfo `json:"output_tokens_details,omitempty"`
}

type OpenAIOutputTokenInfo struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

type OpenAIIncomplete struct {
	Reason string `json:"reason"`
}

type OpenAIOutputItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Summary   json.RawMessage `json:"summary,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type OpenAIMessageContent struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}
