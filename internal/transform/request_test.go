package transform

import (
	"encoding/json"
	"strings"
	"testing"

	"cdx.cc/claude-bridge/internal/config"
	"cdx.cc/claude-bridge/internal/types"
)

func TestDecodeAnthropicRequestAllowsSupportedPassthroughFieldsInStrictMode(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"hello"}],
		"include":["reasoning.encrypted_content"],
		"service_tier":"priority",
		"text":{"format":{"type":"json_schema","strict":true}}
	}`)

	req, err := DecodeAnthropicRequest(body, config.ModeStrict)
	if err != nil {
		t.Fatalf("DecodeAnthropicRequest() error = %v", err)
	}

	if req.ExtraFields == nil {
		t.Fatalf("expected passthrough fields to be retained")
	}
	if _, ok := req.ExtraFields["include"]; !ok {
		t.Fatalf("expected include passthrough field")
	}
	if _, ok := req.ExtraFields["service_tier"]; !ok {
		t.Fatalf("expected service_tier passthrough field")
	}
	if _, ok := req.ExtraFields["text"]; !ok {
		t.Fatalf("expected text passthrough field")
	}
}

func TestDecodeAnthropicRequestStrictPreservesUnknownFieldsForOpenAIPassthrough(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"hello"}],
		"unknown_field":true
	}`)

	req, err := DecodeAnthropicRequest(body, config.ModeStrict)
	if err != nil {
		t.Fatalf("DecodeAnthropicRequest() error = %v", err)
	}
	if string(req.ExtraFields["unknown_field"]) != "true" {
		t.Fatalf("expected unknown field passthrough, got %#v", req.ExtraFields)
	}
}

func TestTransformAnthropicToOpenAIAppliesPassthroughFields(t *testing.T) {
	req := types.AnthropicMessageRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 2048,
		Messages: []types.AnthropicMessage{
			{
				Role:    "user",
				Content: json.RawMessage(`"hello"`),
			},
		},
		Metadata: map[string]any{
			"trace_id": "trace-123",
		},
		Speed: "fast",
		ExtraFields: map[string]json.RawMessage{
			"include":                json.RawMessage(`["reasoning.encrypted_content"]`),
			"parallel_tool_calls":    json.RawMessage(`true`),
			"previous_response_id":   json.RawMessage(`"resp_prev_123"`),
			"prompt_cache_key":       json.RawMessage(`"thread-123"`),
			"prompt_cache_retention": json.RawMessage(`"24h"`),
			"service_tier":           json.RawMessage(`"flex"`),
			"store":                  json.RawMessage(`false`),
			"text": json.RawMessage(`{
				"format":{"type":"json_schema","strict":true,"schema":{"type":"object"},"name":"out"}
			}`),
		},
	}

	oa, err := TransformAnthropicToOpenAI(req, config.ModeStrict, nil)
	if err != nil {
		t.Fatalf("TransformAnthropicToOpenAI() error = %v", err)
	}

	if oa.Metadata["trace_id"] != "trace-123" {
		t.Fatalf("expected metadata to be forwarded, got %#v", oa.Metadata)
	}
	if oa.ServiceTier != "flex" {
		t.Fatalf("expected service_tier passthrough to override speed mapping, got %q", oa.ServiceTier)
	}
	if len(oa.Include) != 1 || oa.Include[0] != "reasoning.encrypted_content" {
		t.Fatalf("unexpected include passthrough: %#v", oa.Include)
	}
	if oa.ParallelToolCalls == nil || !*oa.ParallelToolCalls {
		t.Fatalf("expected parallel_tool_calls passthrough")
	}
	if oa.PreviousResponseID != "resp_prev_123" {
		t.Fatalf("unexpected previous_response_id: %q", oa.PreviousResponseID)
	}
	if oa.PromptCacheKey != "thread-123" {
		t.Fatalf("unexpected prompt_cache_key: %q", oa.PromptCacheKey)
	}
	if oa.PromptCacheRetention != "24h" {
		t.Fatalf("unexpected prompt_cache_retention: %q", oa.PromptCacheRetention)
	}
	if oa.Store == nil || *oa.Store {
		t.Fatalf("expected store=false passthrough")
	}
	if string(oa.Text) == "" {
		t.Fatalf("expected text passthrough to be preserved")
	}
	if len(oa.ExtraFields) != 0 {
		t.Fatalf("expected known passthrough fields to be parsed, got extra=%#v", oa.ExtraFields)
	}
}

func TestToolResultToOutputPreservesStructuredImageResults(t *testing.T) {
	block := types.AnthropicContentBlock{
		Type: "tool_result",
		Content: json.RawMessage(`[
			{"type":"text","text":"caption"},
			{"type":"image","source":{"type":"url","url":"https://example.com/image.png"}}
		]`),
	}

	out, err := toolResultToOutput(block, config.ModeStrict)
	if err != nil {
		t.Fatalf("toolResultToOutput() error = %v", err)
	}

	items, ok := out.([]map[string]any)
	if !ok {
		t.Fatalf("expected structured output, got %T", out)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 structured items, got %d", len(items))
	}
	if items[0]["type"] != "input_text" || items[0]["text"] != "caption" {
		t.Fatalf("unexpected first item: %#v", items[0])
	}
	if items[1]["type"] != "input_image" || items[1]["image_url"] != "https://example.com/image.png" {
		t.Fatalf("unexpected second item: %#v", items[1])
	}
}

func TestMessageToInputItemsConvertsDocumentToInputFile(t *testing.T) {
	msg := types.AnthropicMessage{
		Role: "user",
		Content: json.RawMessage(`[
			{
				"type":"document",
				"name":"manual.pdf",
				"source":{
					"type":"base64",
					"media_type":"application/pdf",
					"data":"SGVsbG8="
				}
			}
		]`),
	}

	items, err := messageToInputItems(msg, config.ModeStrict, map[string]string{})
	if err != nil {
		t.Fatalf("messageToInputItems() error = %v", err)
	}
	if len(items) != 1 || items[0].Type != "message" {
		t.Fatalf("unexpected items: %#v", items)
	}
	if len(items[0].Content) != 1 {
		t.Fatalf("expected one content item, got %#v", items[0].Content)
	}
	content := items[0].Content[0]
	if content.Type != "input_file" {
		t.Fatalf("expected input_file, got %#v", content)
	}
	if content.FileData != "SGVsbG8=" || content.Filename != "manual.pdf" {
		t.Fatalf("unexpected input_file content: %#v", content)
	}
}

func TestToolResultToOutputPreservesDocumentAndSearchResultBlocks(t *testing.T) {
	block := types.AnthropicContentBlock{
		Type: "tool_result",
		Content: json.RawMessage(`[
			{
				"type":"document",
				"name":"notes.pdf",
				"source":{
					"type":"base64",
					"media_type":"application/pdf",
					"data":"SGVsbG8="
				}
			},
			{
				"type":"search_result",
				"title":"Example",
				"url":"https://example.com",
				"text":"Snippet"
			}
		]`),
	}

	out, err := toolResultToOutput(block, config.ModeStrict)
	if err != nil {
		t.Fatalf("toolResultToOutput() error = %v", err)
	}
	items, ok := out.([]map[string]any)
	if !ok {
		t.Fatalf("expected structured output, got %T", out)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 structured items, got %#v", items)
	}
	if items[0]["type"] != "input_file" || items[0]["filename"] != "notes.pdf" {
		t.Fatalf("unexpected document item: %#v", items[0])
	}
	if items[1]["type"] != "input_text" {
		t.Fatalf("unexpected search_result item: %#v", items[1])
	}
	text, _ := items[1]["text"].(string)
	if text == "" || !containsAll(text, []string{"Example", "https://example.com", "Snippet"}) {
		t.Fatalf("unexpected search_result text: %q", text)
	}
}

func TestTransformAnthropicToOpenAIPreservesUnknownOpenAIExtras(t *testing.T) {
	req := types.AnthropicMessageRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 32,
		Messages: []types.AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
		ExtraFields: map[string]json.RawMessage{
			"background":        json.RawMessage(`true`),
			"safety_identifier": json.RawMessage(`"abc-123"`),
		},
	}

	oa, err := TransformAnthropicToOpenAI(req, config.ModeStrict, nil)
	if err != nil {
		t.Fatalf("TransformAnthropicToOpenAI() error = %v", err)
	}
	if string(oa.ExtraFields["background"]) != "true" {
		t.Fatalf("expected background passthrough, got %#v", oa.ExtraFields)
	}
	if string(oa.ExtraFields["safety_identifier"]) != `"abc-123"` {
		t.Fatalf("expected safety_identifier passthrough, got %#v", oa.ExtraFields)
	}
}

func TestMessageToInputItemsPreservesServerToolUseAndCompactionHistory(t *testing.T) {
	msg := types.AnthropicMessage{
		Role: "assistant",
		Content: json.RawMessage(`[
			{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"golang"}},
			{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[]},
			{"type":"compaction","content":"enc_summary"}
		]`),
	}

	items, err := messageToInputItems(msg, config.ModeStrict, map[string]string{})
	if err != nil {
		t.Fatalf("messageToInputItems() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 history items, got %d %#v", len(items), items)
	}
	if items[0].Type != "web_search_call" || items[0].ID != "srv_1" {
		t.Fatalf("unexpected web search history item: %#v", items[0])
	}
	if items[1].Type != "compaction" || items[1].EncryptedContent != "enc_summary" {
		t.Fatalf("unexpected compaction history item: %#v", items[1])
	}
}

func TestToolSearchToolResultMapsToToolSearchOutput(t *testing.T) {
	callKinds := map[string]string{"call_tool_search_1": "tool_search"}
	msg := types.AnthropicMessage{
		Role: "user",
		Content: json.RawMessage(`[
			{
				"type":"tool_result",
				"tool_use_id":"call_tool_search_1",
				"content":[
					{"type":"tool_reference","tool_name":"Bash"},
					{"type":"tool_reference","tool_name":"Read"}
				]
			}
		]`),
	}

	items, err := messageToInputItems(msg, config.ModeStrict, callKinds)
	if err != nil {
		t.Fatalf("messageToInputItems() error = %v", err)
	}
	if len(items) != 1 || items[0].Type != "tool_search_output" {
		t.Fatalf("unexpected tool_search_output mapping: %#v", items)
	}
	if len(items[0].Tools) != 2 {
		t.Fatalf("expected tool_search_output tools, got %#v", items[0].Tools)
	}
}

func TestToolUseMapsBashToLocalShellCallAndToolSearchToToolSearchCall(t *testing.T) {
	msg := types.AnthropicMessage{
		Role: "assistant",
		Content: json.RawMessage(`[
			{"type":"tool_use","id":"call_bash_1","name":"Bash","input":{"command":"pwd"}},
			{"type":"tool_use","id":"call_search_1","name":"ToolSearch","input":{"query":"shell tools","max_results":3}}
		]`),
	}

	items, err := messageToInputItems(msg, config.ModeStrict, map[string]string{})
	if err != nil {
		t.Fatalf("messageToInputItems() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %#v", items)
	}
	if items[0].Type != "local_shell_call" || string(items[0].Action) == "" {
		t.Fatalf("expected local_shell_call mapping, got %#v", items[0])
	}
	if items[1].Type != "tool_search_call" || string(items[1].Arguments) == "" {
		t.Fatalf("expected tool_search_call mapping, got %#v", items[1])
	}
}

func TestImageGenerationCallRoundTripsAsCustomHistoryBlock(t *testing.T) {
	resp := types.OpenAIResponse{
		ID: "resp_image",
		Output: []types.OpenAIOutputItem{
			{
				Type:          "image_generation_call",
				ID:            "ig_123",
				Status:        "completed",
				RevisedPrompt: "a tiny blue square",
				Result:        "Zm9v",
			},
		},
		Status: "completed",
	}

	anth, err := TransformOpenAIToAnthropic(resp, config.ModeStrict, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("TransformOpenAIToAnthropic() error = %v", err)
	}
	if len(anth.Content) != 1 || anth.Content[0].Type != "image_generation_call" {
		t.Fatalf("expected image_generation_call block, got %#v", anth.Content)
	}

	msg := types.AnthropicMessage{
		Role:    "assistant",
		Content: mustMarshalRaw(anth.Content),
	}
	items, err := messageToInputItems(msg, config.ModeStrict, map[string]string{})
	if err != nil {
		t.Fatalf("messageToInputItems() error = %v", err)
	}
	if len(items) != 1 || items[0].Type != "image_generation_call" || items[0].Result != "Zm9v" {
		t.Fatalf("expected image_generation_call input item, got %#v", items)
	}
}

func TestTransformOpenAIToAnthropicMapsCompactionAndServerToolUsage(t *testing.T) {
	resp := types.OpenAIResponse{
		ID: "resp_123",
		Output: []types.OpenAIOutputItem{
			{
				Type:   "web_search_call",
				ID:     "ws_123",
				Action: json.RawMessage(`{"query":"golang"}`),
			},
			{
				Type:             "compaction",
				EncryptedContent: "enc_123",
			},
		},
		Usage: &types.OpenAIUsage{
			InputTokens:  10,
			OutputTokens: 20,
		},
		Status: "completed",
	}

	anth, err := TransformOpenAIToAnthropic(resp, config.ModeStrict, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("TransformOpenAIToAnthropic() error = %v", err)
	}

	if anth.Usage.ServerToolUse == nil || anth.Usage.ServerToolUse.WebSearchRequests != 1 {
		t.Fatalf("expected server_tool_use=1, got %#v", anth.Usage.ServerToolUse)
	}
	foundCompaction := false
	for _, block := range anth.Content {
		if block.Type == "compaction" {
			foundCompaction = true
			if string(block.Content) != `"enc_123"` {
				t.Fatalf("unexpected compaction content: %s", string(block.Content))
			}
		}
	}
	if !foundCompaction {
		t.Fatalf("expected compaction block in response: %#v", anth.Content)
	}
}

func TestTransformOpenAIToAnthropicMapsToolSearchAndLocalShellCalls(t *testing.T) {
	resp := types.OpenAIResponse{
		ID: "resp_tools",
		Output: []types.OpenAIOutputItem{
			{
				Type:      "tool_search_call",
				CallID:    "call_search_1",
				Arguments: json.RawMessage(`{"query":"find shell tools","max_results":5}`),
			},
			{
				Type:   "tool_search_output",
				CallID: "call_search_1",
				Tools: json.RawMessage(`[
					{"type":"function","name":"Bash"},
					{"type":"namespace","name":"fs","description":"","tools":[{"type":"function","name":"Read"}]}
				]`),
			},
			{
				Type:   "local_shell_call",
				CallID: "call_shell_1",
				Action: json.RawMessage(`{"type":"exec","command":["bash","-lc","pwd"]}`),
			},
		},
		Status: "completed",
	}

	anth, err := TransformOpenAIToAnthropic(resp, config.ModeStrict, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("TransformOpenAIToAnthropic() error = %v", err)
	}

	var sawToolSearch, sawToolSearchResult, sawShell bool
	for _, block := range anth.Content {
		switch block.Type {
		case "tool_use":
			switch block.Name {
			case "ToolSearch":
				sawToolSearch = true
			case "Bash":
				sawShell = true
				var input map[string]any
				if err := json.Unmarshal(block.Input, &input); err != nil {
					t.Fatalf("failed to decode shell input: %v", err)
				}
				if input["command"] != "pwd" {
					t.Fatalf("expected unwrapped shell command pwd, got %#v", input)
				}
			}
		case "tool_search_tool_result":
			sawToolSearchResult = true
		}
	}
	if !sawToolSearch || !sawToolSearchResult || !sawShell {
		t.Fatalf("expected mapped tool search + shell blocks, got %#v", anth.Content)
	}
}

func TestCustomToolCallRoundTripsViaResponsesTypeMetadata(t *testing.T) {
	resp := types.OpenAIResponse{
		ID: "resp_custom",
		Output: []types.OpenAIOutputItem{
			{
				Type:   "custom_tool_call",
				CallID: "call_custom_1",
				Name:   "apply_patch",
				Input:  mustMarshalRaw("*** Begin Patch\n*** End Patch\n"),
			},
		},
		Status: "completed",
	}

	anth, err := TransformOpenAIToAnthropic(resp, config.ModeStrict, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("TransformOpenAIToAnthropic() error = %v", err)
	}
	if len(anth.Content) != 1 || anth.Content[0].ResponsesType != "custom_tool_call" {
		t.Fatalf("expected custom_tool_call metadata, got %#v", anth.Content)
	}

	assistantMsg := types.AnthropicMessage{
		Role:    "assistant",
		Content: mustMarshalRaw(anth.Content),
	}
	callKinds := map[string]string{}
	assistantItems, err := messageToInputItems(assistantMsg, config.ModeStrict, callKinds)
	if err != nil {
		t.Fatalf("assistant messageToInputItems() error = %v", err)
	}
	if len(assistantItems) != 1 || assistantItems[0].Type != "custom_tool_call" {
		t.Fatalf("expected custom_tool_call item, got %#v", assistantItems)
	}

	userMsg := types.AnthropicMessage{
		Role: "user",
		Content: json.RawMessage(`[
			{"type":"tool_result","tool_use_id":"call_custom_1","content":"ok"}
		]`),
	}
	userItems, err := messageToInputItems(userMsg, config.ModeStrict, callKinds)
	if err != nil {
		t.Fatalf("user messageToInputItems() error = %v", err)
	}
	if len(userItems) != 1 || userItems[0].Type != "custom_tool_call_output" {
		t.Fatalf("expected custom_tool_call_output, got %#v", userItems)
	}
}

func TestTransformOpenAIToAnthropicPreservesContextManagement(t *testing.T) {
	resp := types.OpenAIResponse{
		ID:                "resp_ctx",
		Status:            "completed",
		ContextManagement: json.RawMessage(`[{"type":"compaction","compact_threshold":160000}]`),
	}

	anth, err := TransformOpenAIToAnthropic(resp, config.ModeStrict, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("TransformOpenAIToAnthropic() error = %v", err)
	}
	if string(anth.ContextManagement) == "" {
		t.Fatalf("expected context_management to be preserved")
	}
}

func TestOpaqueResponsesOutputItemRoundTrips(t *testing.T) {
	item := types.OpenAIOutputItem{
		Type: "mystery_call",
		ID:   "mystery_1",
		ExtraFields: map[string]json.RawMessage{
			"foo":    json.RawMessage(`"bar"`),
			"nested": json.RawMessage(`{"x":1}`),
		},
	}
	resp := types.OpenAIResponse{
		ID:     "resp_opaque",
		Output: []types.OpenAIOutputItem{item},
		Status: "completed",
	}

	anth, err := TransformOpenAIToAnthropic(resp, config.ModeStrict, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("TransformOpenAIToAnthropic() error = %v", err)
	}
	if len(anth.Content) != 1 || anth.Content[0].Type != "responses_output_item" {
		t.Fatalf("expected opaque block, got %#v", anth.Content)
	}

	msg := types.AnthropicMessage{
		Role:    "assistant",
		Content: mustMarshalRaw(anth.Content),
	}
	items, err := messageToInputItems(msg, config.ModeStrict, map[string]string{})
	if err != nil {
		t.Fatalf("messageToInputItems() error = %v", err)
	}
	if len(items) != 1 || items[0].Type != "mystery_call" {
		t.Fatalf("expected opaque item type preserved, got %#v", items)
	}
	if string(items[0].ExtraFields["foo"]) != `"bar"` {
		t.Fatalf("expected opaque extra field preserved, got %#v", items[0].ExtraFields)
	}
}

func TestSpecialResponsesToolItemsRoundTripViaToolUseBlocks(t *testing.T) {
	resp := types.OpenAIResponse{
		ID: "resp_special",
		Output: []types.OpenAIOutputItem{
			{
				Type:    "file_search_call",
				ID:      "fs_item_1",
				Status:  "completed",
				Action:  json.RawMessage(`{"query":"golang bridge","filters":{"scope":"repo"}}`),
				Queries: []string{"golang bridge"},
				Results: json.RawMessage(`[{"file_id":"file_1","file_name":"README.md","score":0.91}]`),
			},
			{
				Type:   "computer_call",
				CallID: "call_computer_1",
				Status: "in_progress",
				Action: json.RawMessage(`{"type":"click","x":120,"y":45}`),
			},
			{
				Type:      "mcp_call",
				ID:        "mcp_item_1",
				Name:      "fetch_doc",
				Status:    "completed",
				Arguments: json.RawMessage(`{"path":"/docs/readme.md"}`),
				Output:    json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`),
				ExtraFields: map[string]json.RawMessage{
					"server_label": json.RawMessage(`"docs-server"`),
					"error":        json.RawMessage(`{"type":"tool_execution_error","message":"none"}`),
				},
			},
			{
				Type: "mcp_list_tools",
				ID:   "mcp_list_1",
				Tools: json.RawMessage(`[
					{"name":"fetch_doc","description":"Read docs","input_schema":{"type":"object"}}
				]`),
				ExtraFields: map[string]json.RawMessage{
					"server_label": json.RawMessage(`"docs-server"`),
				},
			},
		},
		Status: "completed",
	}

	anth, err := TransformOpenAIToAnthropic(resp, config.ModeStrict, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("TransformOpenAIToAnthropic() error = %v", err)
	}
	if len(anth.Content) != 6 {
		t.Fatalf("expected 6 content blocks, got %#v", anth.Content)
	}
	var toolUseCount, toolResultCount int
	var sawFileSearchResult, sawMCPResult bool
	for _, block := range anth.Content {
		switch block.Type {
		case "tool_use":
			toolUseCount++
			if _, ok := block.ExtraFields[storedResponsesItemExtraKey]; !ok {
				t.Fatalf("expected stored response item on block %#v", block)
			}
		case "tool_result":
			toolResultCount++
			if block.ToolUseID == "fs_item_1" {
				sawFileSearchResult = true
			}
			if block.ToolUseID == "mcp_item_1" {
				sawMCPResult = true
			}
		default:
			t.Fatalf("unexpected content block %#v", block)
		}
	}
	if toolUseCount != 4 || toolResultCount != 2 || !sawFileSearchResult || !sawMCPResult {
		t.Fatalf("expected 4 tool_use + 2 tool_result blocks, got %#v", anth.Content)
	}

	msg := types.AnthropicMessage{
		Role:    "assistant",
		Content: mustMarshalRaw(anth.Content),
	}
	items, err := messageToInputItems(msg, config.ModeStrict, map[string]string{})
	if err != nil {
		t.Fatalf("messageToInputItems() error = %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 input items, got %#v", items)
	}
	if items[0].Type != "file_search_call" || string(items[0].Action) == "" {
		t.Fatalf("expected file_search_call action preserved, got %#v", items[0])
	}
	if len(items[0].Queries) != 1 || items[0].Queries[0] != "golang bridge" || string(items[0].Results) == "" {
		t.Fatalf("expected file_search metadata preserved, got %#v", items[0])
	}
	if items[1].Type != "computer_call" || items[1].CallID != "call_computer_1" {
		t.Fatalf("expected computer_call preserved, got %#v", items[1])
	}
	if items[2].Type != "mcp_call" || items[2].Name != "fetch_doc" || string(items[2].ExtraFields["server_label"]) != `"docs-server"` {
		t.Fatalf("expected mcp_call preserved, got %#v", items[2])
	}
	if string(items[2].Error) == "" || string(items[2].Output.(json.RawMessage)) == "" {
		t.Fatalf("expected mcp_call error/output preserved, got %#v", items[2])
	}
	if items[3].Type != "mcp_list_tools" || items[3].ID != "mcp_list_1" || len(items[3].Tools) != 1 {
		t.Fatalf("expected mcp_list_tools preserved, got %#v", items[3])
	}
}

func TestComputerCallOutputRoundTripsViaToolResult(t *testing.T) {
	resp := types.OpenAIResponse{
		ID: "resp_computer_output",
		Output: []types.OpenAIOutputItem{
			{
				Type:   "computer_call",
				CallID: "call_computer_1",
				Status: "completed",
				Action: json.RawMessage(`{"type":"click","x":12,"y":34}`),
			},
			{
				Type:                     "computer_call_output",
				CallID:                   "call_computer_1",
				Status:                   "completed",
				Output:                   json.RawMessage(`{"type":"computer_screenshot","image_url":"https://example.com/screen.png"}`),
				AcknowledgedSafetyChecks: json.RawMessage(`[{"id":"check_1"}]`),
			},
		},
		Status: "completed",
	}

	anth, err := TransformOpenAIToAnthropic(resp, config.ModeStrict, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("TransformOpenAIToAnthropic() error = %v", err)
	}
	if len(anth.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %#v", anth.Content)
	}
	if anth.Content[1].Type != "tool_result" || anth.Content[1].ToolUseID != "call_computer_1" {
		t.Fatalf("expected computer tool_result block, got %#v", anth.Content[1])
	}

	callKinds := map[string]string{}
	assistantMsg := types.AnthropicMessage{
		Role:    "assistant",
		Content: mustMarshalRaw(anth.Content[:1]),
	}
	assistantItems, err := messageToInputItems(assistantMsg, config.ModeStrict, callKinds)
	if err != nil {
		t.Fatalf("assistant messageToInputItems() error = %v", err)
	}
	if len(assistantItems) != 1 || assistantItems[0].Type != "computer_call" {
		t.Fatalf("expected computer_call item, got %#v", assistantItems)
	}

	userMsg := types.AnthropicMessage{
		Role:    "user",
		Content: mustMarshalRaw([]types.AnthropicContentBlock{anth.Content[1]}),
	}
	userItems, err := messageToInputItems(userMsg, config.ModeStrict, callKinds)
	if err != nil {
		t.Fatalf("user messageToInputItems() error = %v", err)
	}
	if len(userItems) != 1 || userItems[0].Type != "computer_call_output" {
		t.Fatalf("expected computer_call_output item, got %#v", userItems)
	}
	outputMap, ok := userItems[0].Output.(map[string]any)
	if !ok || outputMap["type"] != "computer_screenshot" || outputMap["image_url"] != "https://example.com/screen.png" {
		t.Fatalf("unexpected computer_call_output payload: %#v", userItems[0].Output)
	}
	if string(userItems[0].ExtraFields["acknowledged_safety_checks"]) != `[{"id":"check_1"}]` {
		t.Fatalf("expected safety checks to survive, got %#v", userItems[0].ExtraFields)
	}
}

func TestRefusalRoundTripsViaTextMetadata(t *testing.T) {
	resp := types.OpenAIResponse{
		ID: "resp_refusal",
		Output: []types.OpenAIOutputItem{
			{
				Type:    "message",
				ID:      "msg_refusal_1",
				Role:    "assistant",
				Content: json.RawMessage(`[{"type":"refusal","refusal":"nope"}]`),
			},
		},
		Status: "completed",
	}

	anth, err := TransformOpenAIToAnthropic(resp, config.ModeStrict, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("TransformOpenAIToAnthropic() error = %v", err)
	}
	if len(anth.Content) != 1 || anth.Content[0].Type != "text" || anth.Content[0].ResponsesType != "refusal" {
		t.Fatalf("expected refusal text metadata, got %#v", anth.Content)
	}

	msg := types.AnthropicMessage{
		Role:    "assistant",
		Content: mustMarshalRaw(anth.Content),
	}
	items, err := messageToInputItems(msg, config.ModeStrict, map[string]string{})
	if err != nil {
		t.Fatalf("messageToInputItems() error = %v", err)
	}
	if len(items) != 1 || len(items[0].Content) != 1 || items[0].Content[0].Type != "refusal" || items[0].Content[0].Refusal != "nope" {
		t.Fatalf("expected refusal round-trip, got %#v", items)
	}
}

func TestSpecialToolUseFallbackParsesQueriesResultsAndOutput(t *testing.T) {
	msg := types.AnthropicMessage{
		Role: "assistant",
		Content: json.RawMessage(`[
			{
				"type":"tool_use",
				"id":"fs_1",
				"name":"FileSearch",
				"responses_type":"file_search_call",
				"input":{
					"queries":["bridge protocol"],
					"results":[{"file_id":"file_1","file_name":"README.md"}]
				}
			},
			{
				"type":"tool_use",
				"id":"mcp_1",
				"name":"MCP",
				"responses_type":"mcp_call",
				"input":{
					"server_label":"docs-server",
					"output":{"content":[{"type":"text","text":"ok"}]},
					"error":{"type":"tool_execution_error","message":"none"}
				}
			}
		]`),
	}

	items, err := messageToInputItems(msg, config.ModeStrict, map[string]string{})
	if err != nil {
		t.Fatalf("messageToInputItems() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %#v", items)
	}
	if items[0].Type != "file_search_call" || len(items[0].Queries) != 1 || items[0].Queries[0] != "bridge protocol" || string(items[0].Results) == "" {
		t.Fatalf("expected file_search fallback fields, got %#v", items[0])
	}
	if items[1].Type != "mcp_call" || string(items[1].Error) == "" || items[1].Output == nil || string(items[1].ExtraFields["server_label"]) != `"docs-server"` {
		t.Fatalf("expected mcp fallback fields, got %#v", items[1])
	}
}

func containsAll(text string, parts []string) bool {
	for _, part := range parts {
		if !strings.Contains(text, part) {
			return false
		}
	}
	return true
}
