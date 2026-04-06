package types

import "encoding/json"

type OpenAIReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type OpenAIResponsesRequest struct {
	Model                string                     `json:"model"`
	Input                []OpenAIInputItem          `json:"input,omitempty"`
	Instructions         string                     `json:"instructions,omitempty"`
	Tools                []OpenAITool               `json:"tools,omitempty"`
	ToolChoice           any                        `json:"tool_choice,omitempty"`
	ParallelToolCalls    *bool                      `json:"parallel_tool_calls,omitempty"`
	Stream               bool                       `json:"stream,omitempty"`
	MaxOutputTokens      *int                       `json:"max_output_tokens,omitempty"`
	Temperature          *float64                   `json:"temperature,omitempty"`
	TopP                 *float64                   `json:"top_p,omitempty"`
	Stop                 []string                   `json:"stop,omitempty"`
	Include              []string                   `json:"include,omitempty"`
	Metadata             map[string]any             `json:"metadata,omitempty"`
	Store                *bool                      `json:"store,omitempty"`
	PreviousResponseID   string                     `json:"previous_response_id,omitempty"`
	Reasoning            *OpenAIReasoning           `json:"reasoning,omitempty"`
	ServiceTier          string                     `json:"service_tier,omitempty"`
	PromptCacheKey       string                     `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention string                     `json:"prompt_cache_retention,omitempty"`
	Text                 json.RawMessage            `json:"text,omitempty"`
	ContextManagement    json.RawMessage            `json:"context_management,omitempty"` // 服务端 compaction
	ExtraFields          map[string]json.RawMessage `json:"-"`
}

type OpenAIInputItem struct {
	Type             string                     `json:"type,omitempty"`
	ID               string                     `json:"id,omitempty"`
	Role             string                     `json:"role,omitempty"`
	Content          []OpenAIInputContent       `json:"content,omitempty"`
	CallID           string                     `json:"call_id,omitempty"`
	Name             string                     `json:"name,omitempty"`
	Namespace        string                     `json:"namespace,omitempty"`
	Arguments        json.RawMessage            `json:"arguments,omitempty"`
	Input            json.RawMessage            `json:"input,omitempty"`
	Error            json.RawMessage            `json:"error,omitempty"`
	Output           any                        `json:"output,omitempty"`
	Status           string                     `json:"status,omitempty"`
	Execution        string                     `json:"execution,omitempty"`
	Action           json.RawMessage            `json:"action,omitempty"`
	Tools            []any                      `json:"tools,omitempty"`
	Queries          []string                   `json:"queries,omitempty"`
	Results          json.RawMessage            `json:"results,omitempty"`
	EncryptedContent string                     `json:"encrypted_content,omitempty"`
	RevisedPrompt    string                     `json:"revised_prompt,omitempty"`
	Result           string                     `json:"result,omitempty"`
	Phase            string                     `json:"phase,omitempty"`
	EndTurn          *bool                      `json:"end_turn,omitempty"`
	ExtraFields      map[string]json.RawMessage `json:"-"`
}

type OpenAIInputContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Refusal  string `json:"refusal,omitempty"`
	ImageURL string `json:"image_url,omitempty"` // Responses API: 平铺字符串，非嵌套对象
	Detail   string `json:"detail,omitempty"`    // input_image 的细节级别
	FileID   string `json:"file_id,omitempty"`
	FileData string `json:"file_data,omitempty"`
	FileURL  string `json:"file_url,omitempty"`
	Filename string `json:"filename,omitempty"`
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
	ContextManagement json.RawMessage    `json:"context_management,omitempty"`
}

type OpenAIUsage struct {
	InputTokens        int                    `json:"input_tokens"`
	InputTokenDetails  *OpenAIInputTokenInfo  `json:"input_tokens_details,omitempty"`
	OutputTokens       int                    `json:"output_tokens"`
	OutputTokenDetails *OpenAIOutputTokenInfo `json:"output_tokens_details,omitempty"`
	TotalTokens        int                    `json:"total_tokens,omitempty"`
}

type OpenAIInputTokenInfo struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

type OpenAIOutputTokenInfo struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

type OpenAIIncomplete struct {
	Reason string `json:"reason"`
}

type OpenAIOutputItem struct {
	Type                     string                     `json:"type"`
	ID                       string                     `json:"id,omitempty"`
	Role                     string                     `json:"role,omitempty"`
	Content                  json.RawMessage            `json:"content,omitempty"`
	Summary                  json.RawMessage            `json:"summary,omitempty"`
	CallID                   string                     `json:"call_id,omitempty"`
	Name                     string                     `json:"name,omitempty"`
	Namespace                string                     `json:"namespace,omitempty"`
	Arguments                json.RawMessage            `json:"arguments,omitempty"`
	Input                    json.RawMessage            `json:"input,omitempty"`
	Output                   json.RawMessage            `json:"output,omitempty"`
	Error                    json.RawMessage            `json:"error,omitempty"`
	Action                   json.RawMessage            `json:"action,omitempty"`                     // web_search_call
	Status                   string                     `json:"status,omitempty"`                     // web_search_call
	Execution                string                     `json:"execution,omitempty"`                  // tool_search_call
	Tools                    json.RawMessage            `json:"tools,omitempty"`                      // tool_search_output
	Queries                  []string                   `json:"queries,omitempty"`                    // file_search_call
	Results                  json.RawMessage            `json:"results,omitempty"`                    // file_search_call
	AcknowledgedSafetyChecks json.RawMessage            `json:"acknowledged_safety_checks,omitempty"` // computer_call_output
	EncryptedContent         string                     `json:"encrypted_content,omitempty"`          // compaction
	RevisedPrompt            string                     `json:"revised_prompt,omitempty"`             // image_generation_call
	Result                   string                     `json:"result,omitempty"`                     // image_generation_call
	Phase                    string                     `json:"phase,omitempty"`                      // message
	EndTurn                  *bool                      `json:"end_turn,omitempty"`                   // message
	ExtraFields              map[string]json.RawMessage `json:"-"`
}

type OpenAIAnnotation struct {
	Type       string `json:"type"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	StartIndex int    `json:"start_index,omitempty"`
	EndIndex   int    `json:"end_index,omitempty"`
}

type OpenAIMessageContent struct {
	Type        string             `json:"type"`
	Text        string             `json:"text,omitempty"`
	Refusal     string             `json:"refusal,omitempty"`
	Annotations []OpenAIAnnotation `json:"annotations,omitempty"`
}

func (r OpenAIResponsesRequest) MarshalJSON() ([]byte, error) {
	type alias OpenAIResponsesRequest
	base, err := json.Marshal(alias(r))
	if err != nil {
		return nil, err
	}
	if len(r.ExtraFields) == 0 {
		return base, nil
	}

	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for key, val := range r.ExtraFields {
		if len(val) == 0 {
			continue
		}
		merged[key] = append(json.RawMessage(nil), val...)
	}
	return json.Marshal(merged)
}

func (i OpenAIInputItem) MarshalJSON() ([]byte, error) {
	type alias OpenAIInputItem
	base, err := json.Marshal(alias(i))
	if err != nil {
		return nil, err
	}
	if len(i.ExtraFields) == 0 {
		return base, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for key, val := range i.ExtraFields {
		if len(val) == 0 {
			continue
		}
		merged[key] = append(json.RawMessage(nil), val...)
	}
	return json.Marshal(merged)
}

func (i OpenAIOutputItem) MarshalJSON() ([]byte, error) {
	type alias OpenAIOutputItem
	base, err := json.Marshal(alias(i))
	if err != nil {
		return nil, err
	}
	if len(i.ExtraFields) == 0 {
		return base, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for key, val := range i.ExtraFields {
		if len(val) == 0 {
			continue
		}
		merged[key] = append(json.RawMessage(nil), val...)
	}
	return json.Marshal(merged)
}

func (i *OpenAIOutputItem) UnmarshalJSON(data []byte) error {
	type alias OpenAIOutputItem
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
		"id",
		"role",
		"content",
		"summary",
		"call_id",
		"name",
		"namespace",
		"arguments",
		"input",
		"output",
		"error",
		"action",
		"status",
		"execution",
		"tools",
		"queries",
		"results",
		"acknowledged_safety_checks",
		"encrypted_content",
		"revised_prompt",
		"result",
		"phase",
		"end_turn",
	} {
		delete(raw, key)
	}
	*i = OpenAIOutputItem(parsed)
	if len(raw) > 0 {
		i.ExtraFields = raw
	}
	return nil
}
