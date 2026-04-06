package transform

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"cdx.cc/claude-bridge/internal/config"
	"cdx.cc/claude-bridge/internal/sse"
)

func TestBridgeOpenAIStreamEmitsCitationsDeltaFromContentPartDone(t *testing.T) {
	events := make(chan sse.Event, 8)
	events <- sse.Event{
		Name: "response.created",
		Data: []byte(`{"type":"response.created","response":{"id":"resp_1"}}`),
	}
	events <- sse.Event{
		Name: "response.content_part.added",
		Data: []byte(`{
			"type":"response.content_part.added",
			"item_id":"item_1",
			"output_index":0,
			"content_index":0,
			"part":{"type":"output_text"}
		}`),
	}
	events <- sse.Event{
		Name: "response.output_text.delta",
		Data: []byte(`{
			"type":"response.output_text.delta",
			"item_id":"item_1",
			"output_index":0,
			"content_index":0,
			"delta":"hello world"
		}`),
	}
	events <- sse.Event{
		Name: "response.content_part.done",
		Data: []byte(`{
			"type":"response.content_part.done",
			"item_id":"item_1",
			"output_index":0,
			"content_index":0,
			"part":{
				"type":"output_text",
				"text":"hello world",
				"annotations":[
					{
						"type":"url_citation",
						"url":"https://example.com",
						"title":"Example",
						"start_index":0,
						"end_index":5
					}
				]
			}
		}`),
	}
	events <- sse.Event{
		Name: "response.completed",
		Data: []byte(`{
			"type":"response.completed",
			"response":{
				"id":"resp_1",
				"status":"completed",
				"usage":{"input_tokens":10,"output_tokens":2}
			}
		}`),
	}
	close(events)

	var buf bytes.Buffer
	writer := sse.NewWriter(&buf, nil)
	if err := BridgeOpenAIStream(context.Background(), events, writer, config.ModeStrict, "claude-sonnet-4-6", 10, "priority", "24h"); err != nil {
		t.Fatalf("BridgeOpenAIStream() error = %v", err)
	}

	gotEvents := readSSEEvents(t, buf.Bytes())
	var citationFound bool
	for _, ev := range gotEvents {
		if ev.Name != "content_block_delta" {
			continue
		}
		var payload struct {
			Delta map[string]any `json:"delta"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("unmarshal delta event: %v", err)
		}
		if payload.Delta["type"] != "citations_delta" {
			continue
		}
		citation, _ := payload.Delta["citation"].(map[string]any)
		if citation["url"] != "https://example.com" {
			t.Fatalf("unexpected citation payload: %#v", citation)
		}
		citationFound = true
	}
	if !citationFound {
		t.Fatalf("expected citations_delta in stream, got %s", buf.String())
	}
}

func TestBridgeOpenAIStreamBackfillsCacheCreationUsage(t *testing.T) {
	events := make(chan sse.Event, 3)
	events <- sse.Event{
		Name: "response.created",
		Data: []byte(`{"type":"response.created","response":{"id":"resp_cache"}}`),
	}
	events <- sse.Event{
		Name: "response.completed",
		Data: []byte(`{
			"type":"response.completed",
			"response":{
				"id":"resp_cache",
				"status":"completed",
				"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":7}}
			}
		}`),
	}
	close(events)

	var buf bytes.Buffer
	writer := sse.NewWriter(&buf, nil)
	if err := BridgeOpenAIStream(context.Background(), events, writer, config.ModeStrict, "claude-sonnet-4-6", 10, "standard", "24h"); err != nil {
		t.Fatalf("BridgeOpenAIStream() error = %v", err)
	}

	gotEvents := readSSEEvents(t, buf.Bytes())
	for _, ev := range gotEvents {
		if ev.Name != "message_delta" {
			continue
		}
		var payload struct {
			Usage *struct {
				CacheReadInputTokens int `json:"cache_read_input_tokens"`
				CacheCreation        *struct {
					Ephemeral1HInputTokens int `json:"ephemeral_1h_input_tokens"`
				} `json:"cache_creation"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("unmarshal message_delta: %v", err)
		}
		if payload.Usage == nil || payload.Usage.CacheReadInputTokens != 7 || payload.Usage.CacheCreation == nil || payload.Usage.CacheCreation.Ephemeral1HInputTokens != 7 {
			t.Fatalf("expected cache usage backfill, got %#v", payload.Usage)
		}
		return
	}
	t.Fatalf("expected message_delta event, got %s", buf.String())
}

func TestBridgeOpenAIStreamHandlesMcpCallArgumentDeltas(t *testing.T) {
	events := make(chan sse.Event, 8)
	events <- sse.Event{
		Name: "response.created",
		Data: []byte(`{"type":"response.created","response":{"id":"resp_mcp"}}`),
	}
	events <- sse.Event{
		Name: "response.output_item.added",
		Data: []byte(`{
			"type":"response.output_item.added",
			"output_index":0,
			"item":{"type":"mcp_call","id":"mcp_item_1","name":"fetch_doc"}
		}`),
	}
	events <- sse.Event{
		Name: "response.mcp_call_arguments.delta",
		Data: []byte(`{
			"type":"response.mcp_call_arguments.delta",
			"item_id":"mcp_item_1",
			"output_index":0,
			"delta":"{\"path\":\"/docs/readme.md\"}"
		}`),
	}
	events <- sse.Event{
		Name: "response.mcp_call_arguments.done",
		Data: []byte(`{
			"type":"response.mcp_call_arguments.done",
			"item_id":"mcp_item_1",
			"output_index":0
		}`),
	}
	events <- sse.Event{
		Name: "response.completed",
		Data: []byte(`{
			"type":"response.completed",
			"response":{"id":"resp_mcp","status":"completed","usage":{"input_tokens":12,"output_tokens":3}}
		}`),
	}
	close(events)

	var buf bytes.Buffer
	writer := sse.NewWriter(&buf, nil)
	if err := BridgeOpenAIStream(context.Background(), events, writer, config.ModeStrict, "claude-sonnet-4-6", 12, "standard", "24h"); err != nil {
		t.Fatalf("BridgeOpenAIStream() error = %v", err)
	}

	gotEvents := readSSEEvents(t, buf.Bytes())
	var sawToolUse, sawDelta bool
	for _, ev := range gotEvents {
		switch ev.Name {
		case "content_block_start":
			var payload struct {
				ContentBlock map[string]any `json:"content_block"`
			}
			if err := json.Unmarshal(ev.Data, &payload); err != nil {
				t.Fatalf("unmarshal start event: %v", err)
			}
			if payload.ContentBlock["type"] == "tool_use" && payload.ContentBlock["responses_type"] == "mcp_call" {
				sawToolUse = true
			}
		case "content_block_delta":
			var payload struct {
				Delta map[string]any `json:"delta"`
			}
			if err := json.Unmarshal(ev.Data, &payload); err != nil {
				t.Fatalf("unmarshal delta event: %v", err)
			}
			if payload.Delta["type"] == "input_json_delta" {
				sawDelta = true
			}
		}
	}
	if !sawToolUse || !sawDelta {
		t.Fatalf("expected streamed mcp tool_use + input_json_delta, got %s", buf.String())
	}
}

func TestBridgeOpenAIStreamEmitsSpecialToolUseFromDone(t *testing.T) {
	events := make(chan sse.Event, 4)
	events <- sse.Event{
		Name: "response.created",
		Data: []byte(`{"type":"response.created","response":{"id":"resp_special"}}`),
	}
	events <- sse.Event{
		Name: "response.output_item.done",
		Data: []byte(`{
			"type":"response.output_item.done",
			"item":{
				"type":"file_search_call",
				"id":"fs_item_1",
				"status":"completed",
				"action":{"query":"bridge"},
				"queries":["bridge"],
				"results":[{"file_id":"file_1","file_name":"README.md","score":0.9}]
			}
		}`),
	}
	events <- sse.Event{
		Name: "response.completed",
		Data: []byte(`{
			"type":"response.completed",
			"response":{"id":"resp_special","status":"completed","usage":{"input_tokens":8,"output_tokens":2}}
		}`),
	}
	close(events)

	var buf bytes.Buffer
	writer := sse.NewWriter(&buf, nil)
	if err := BridgeOpenAIStream(context.Background(), events, writer, config.ModeStrict, "claude-sonnet-4-6", 8, "standard", "24h"); err != nil {
		t.Fatalf("BridgeOpenAIStream() error = %v", err)
	}

	gotEvents := readSSEEvents(t, buf.Bytes())
	var foundToolUse, foundToolResult bool
	for _, ev := range gotEvents {
		if ev.Name != "content_block_start" {
			continue
		}
		var payload struct {
			ContentBlock struct {
				Type          string          `json:"type"`
				Name          string          `json:"name"`
				ResponsesType string          `json:"responses_type"`
				ToolUseID     string          `json:"tool_use_id"`
				Content       json.RawMessage `json:"content"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("unmarshal content_block_start: %v", err)
		}
		if payload.ContentBlock.Type == "tool_use" && payload.ContentBlock.ResponsesType == "file_search_call" {
			foundToolUse = true
		}
		if payload.ContentBlock.Type == "tool_result" && payload.ContentBlock.ToolUseID == "fs_item_1" {
			foundToolResult = true
		}
	}
	if !foundToolUse || !foundToolResult {
		t.Fatalf("expected file_search_call to stream as tool_use + tool_result, got %s", buf.String())
	}
}

func TestBridgeOpenAIStreamUsesAddedAndDoneForFileSearchWithoutDuplicateToolUse(t *testing.T) {
	events := make(chan sse.Event, 5)
	events <- sse.Event{
		Name: "response.created",
		Data: []byte(`{"type":"response.created","response":{"id":"resp_file_search"}}`),
	}
	events <- sse.Event{
		Name: "response.output_item.added",
		Data: []byte(`{
			"type":"response.output_item.added",
			"output_index":0,
			"item":{"type":"file_search_call","id":"fs_item_2","status":"searching","queries":["bridge"]}
		}`),
	}
	events <- sse.Event{
		Name: "response.output_item.done",
		Data: []byte(`{
			"type":"response.output_item.done",
			"item":{
				"type":"file_search_call",
				"id":"fs_item_2",
				"status":"completed",
				"queries":["bridge"],
				"results":[{"file_id":"file_1","file_name":"README.md","score":0.9}]
			}
		}`),
	}
	events <- sse.Event{
		Name: "response.completed",
		Data: []byte(`{
			"type":"response.completed",
			"response":{"id":"resp_file_search","status":"completed","usage":{"input_tokens":8,"output_tokens":2}}
		}`),
	}
	close(events)

	var buf bytes.Buffer
	writer := sse.NewWriter(&buf, nil)
	if err := BridgeOpenAIStream(context.Background(), events, writer, config.ModeStrict, "claude-sonnet-4-6", 8, "standard", "24h"); err != nil {
		t.Fatalf("BridgeOpenAIStream() error = %v", err)
	}

	gotEvents := readSSEEvents(t, buf.Bytes())
	var toolUseCount, toolResultCount int
	for _, ev := range gotEvents {
		if ev.Name != "content_block_start" {
			continue
		}
		var payload struct {
			ContentBlock struct {
				Type          string `json:"type"`
				ResponsesType string `json:"responses_type"`
				ToolUseID     string `json:"tool_use_id"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("unmarshal content_block_start: %v", err)
		}
		if payload.ContentBlock.Type == "tool_use" && payload.ContentBlock.ResponsesType == "file_search_call" {
			toolUseCount++
		}
		if payload.ContentBlock.Type == "tool_result" && payload.ContentBlock.ToolUseID == "fs_item_2" {
			toolResultCount++
		}
	}
	if toolUseCount != 1 || toolResultCount != 1 {
		t.Fatalf("expected 1 file_search tool_use + 1 tool_result, got tool_use=%d tool_result=%d raw=%s", toolUseCount, toolResultCount, buf.String())
	}
}

func TestBridgeOpenAIStreamEmitsComputerCallOutputAsToolResult(t *testing.T) {
	events := make(chan sse.Event, 4)
	events <- sse.Event{
		Name: "response.created",
		Data: []byte(`{"type":"response.created","response":{"id":"resp_computer"}}`),
	}
	events <- sse.Event{
		Name: "response.output_item.done",
		Data: []byte(`{
			"type":"response.output_item.done",
			"item":{
				"type":"computer_call_output",
				"call_id":"call_computer_1",
				"status":"completed",
				"output":{"type":"computer_screenshot","image_url":"https://example.com/screen.png"}
			}
		}`),
	}
	events <- sse.Event{
		Name: "response.completed",
		Data: []byte(`{
			"type":"response.completed",
			"response":{"id":"resp_computer","status":"completed","usage":{"input_tokens":8,"output_tokens":2}}
		}`),
	}
	close(events)

	var buf bytes.Buffer
	writer := sse.NewWriter(&buf, nil)
	if err := BridgeOpenAIStream(context.Background(), events, writer, config.ModeStrict, "claude-sonnet-4-6", 8, "standard", "24h"); err != nil {
		t.Fatalf("BridgeOpenAIStream() error = %v", err)
	}

	gotEvents := readSSEEvents(t, buf.Bytes())
	var found bool
	for _, ev := range gotEvents {
		if ev.Name != "content_block_start" {
			continue
		}
		var payload struct {
			ContentBlock struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("unmarshal content_block_start: %v", err)
		}
		if payload.ContentBlock.Type != "tool_result" || payload.ContentBlock.ToolUseID != "call_computer_1" {
			continue
		}
		found = true
		var blocks []map[string]any
		if err := json.Unmarshal(payload.ContentBlock.Content, &blocks); err != nil {
			t.Fatalf("unmarshal tool_result content: %v", err)
		}
		if len(blocks) != 1 || blocks[0]["type"] != "image" {
			t.Fatalf("unexpected computer tool_result content: %#v", blocks)
		}
	}
	if !found {
		t.Fatalf("expected computer_call_output to stream as tool_result, got %s", buf.String())
	}
}

func TestBridgeOpenAIStreamMarksRefusalTextBlocks(t *testing.T) {
	events := make(chan sse.Event, 4)
	events <- sse.Event{
		Name: "response.created",
		Data: []byte(`{"type":"response.created","response":{"id":"resp_refusal"}}`),
	}
	events <- sse.Event{
		Name: "response.refusal.delta",
		Data: []byte(`{
			"type":"response.refusal.delta",
			"item_id":"msg_1",
			"output_index":0,
			"content_index":0,
			"delta":"nope"
		}`),
	}
	events <- sse.Event{
		Name: "response.refusal.done",
		Data: []byte(`{
			"type":"response.refusal.done",
			"item_id":"msg_1",
			"output_index":0,
			"content_index":0
		}`),
	}
	events <- sse.Event{
		Name: "response.completed",
		Data: []byte(`{
			"type":"response.completed",
			"response":{"id":"resp_refusal","status":"completed","usage":{"input_tokens":8,"output_tokens":1}}
		}`),
	}
	close(events)

	var buf bytes.Buffer
	writer := sse.NewWriter(&buf, nil)
	if err := BridgeOpenAIStream(context.Background(), events, writer, config.ModeBestEffort, "claude-sonnet-4-6", 8, "standard", "24h"); err != nil {
		t.Fatalf("BridgeOpenAIStream() error = %v", err)
	}

	gotEvents := readSSEEvents(t, buf.Bytes())
	var found bool
	for _, ev := range gotEvents {
		if ev.Name != "content_block_start" {
			continue
		}
		var payload struct {
			ContentBlock struct {
				Type          string `json:"type"`
				ResponsesType string `json:"responses_type"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("unmarshal content_block_start: %v", err)
		}
		if payload.ContentBlock.Type == "text" && payload.ContentBlock.ResponsesType == "refusal" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected refusal stream block metadata, got %s", buf.String())
	}
}

func readSSEEvents(t *testing.T, raw []byte) []sse.Event {
	t.Helper()
	events, errs := sse.Read(bytes.NewReader(raw))
	var out []sse.Event
	for ev := range events {
		out = append(out, ev)
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("read SSE events: %v", err)
		}
	}
	return out
}
