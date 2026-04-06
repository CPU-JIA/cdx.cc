package transform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"cdx.cc/claude-bridge/internal/config"
	"cdx.cc/claude-bridge/internal/sse"
	"cdx.cc/claude-bridge/internal/types"
)

type webSearchInfo struct {
	toolUseID     string
	resultEmitted bool
}

type streamState struct {
	started              bool
	messageID            string
	model                string
	serviceTier          string
	speed                string
	promptCacheRetention string
	nextIndex            int
	textIndexByKey       map[string]int
	textValueByKey       map[string]string
	toolIndexByItemID    map[string]int
	toolIndexByOutput    map[int]int
	closedIndex          map[int]bool
	toolUsed             bool
	thinkingIndex        int  // thinking block 索引，-1 = 未创建
	thinkingStarted      bool // 是否已发送 thinking content_block_start
	webSearchByItemID    map[string]*webSearchInfo
	webSearchByOutIdx    map[int]*webSearchInfo
	estimatedInputTokens int // 估算的 input token 数
	serverToolUseCount   int
}

func newStreamState() *streamState {
	return &streamState{
		textIndexByKey:    make(map[string]int),
		textValueByKey:    make(map[string]string),
		toolIndexByItemID: make(map[string]int),
		toolIndexByOutput: make(map[int]int),
		closedIndex:       make(map[int]bool),
		thinkingIndex:     -1,
		webSearchByItemID: make(map[string]*webSearchInfo),
		webSearchByOutIdx: make(map[int]*webSearchInfo),
	}
}

func BridgeOpenAIStream(ctx context.Context, reader <-chan sse.Event, writer *sse.Writer, mode config.Mode, requestModel string, estimatedInputTokens int, serviceTier string, promptCacheRetention string) error {
	state := newStreamState()
	state.model = requestModel
	state.estimatedInputTokens = estimatedInputTokens
	state.serviceTier = anthropicServiceTier(serviceTier)
	state.speed = anthropicSpeed(serviceTier)
	state.promptCacheRetention = strings.TrimSpace(strings.ToLower(promptCacheRetention))

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-reader:
			if !ok {
				return nil
			}
			if err := handleOpenAIEvent(event, state, writer, mode); err != nil {
				return err
			}
		}
	}
}

func handleOpenAIEvent(event sse.Event, state *streamState, writer *sse.Writer, mode config.Mode) error {
	if len(event.Data) == 0 {
		return nil
	}

	// OpenAI 流以 [DONE] 结束，非 JSON，直接跳过
	if string(event.Data) == "[DONE]" {
		return nil
	}

	// Sub2API has inconsistent event names vs data types, so we check the data type field
	var typeCheck struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(event.Data, &typeCheck); err != nil {
		log.Printf("WARN: Skipping non-JSON event data: %v, data=%s", err, truncate(event.Data, 200))
		return nil
	}
	if typeCheck.Type != "" {
		// Override event name with actual data type
		event.Name = typeCheck.Type
	}

	switch event.Name {
	case "response.created", "response.in_progress":
		meta := struct {
			Response *types.OpenAIResponse `json:"response"`
		}{}
		if err := json.Unmarshal(event.Data, &meta); err != nil {
			return nil
		}
		if meta.Response != nil {
			// model 保持请求时的原始 Claude 模型名，不被上游覆盖
			state.messageID = "msg_" + meta.Response.ID
		}
		return ensureMessageStart(state, writer)
	case "response.output_item.added":
		var payload struct {
			OutputIndex int                    `json:"output_index"`
			Item        types.OpenAIOutputItem `json:"item"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return err
		}
		if err := ensureMessageStart(state, writer); err != nil {
			return err
		}
		switch payload.Item.Type {
		case "function_call", "custom_tool_call":
			idx := state.nextIndex
			state.nextIndex++
			state.toolIndexByItemID[payload.Item.ID] = idx
			if payload.OutputIndex >= 0 {
				state.toolIndexByOutput[payload.OutputIndex] = idx
			}
			state.toolUsed = true
			block := types.AnthropicContentBlock{
				Type:  "tool_use",
				ID:    payload.Item.CallID,
				Name:  payload.Item.Name,
				Input: mustMarshalRaw(map[string]any{}),
			}
			start := types.AnthropicStreamContentBlockStart{
				Type:         "content_block_start",
				Index:        idx,
				ContentBlock: block,
			}
			return sendEvent(writer, "content_block_start", start)
		case "local_shell_call", "tool_search_call", "file_search_call", "computer_call", "mcp_list_tools":
			block, err := toolUseBlockFromAddedItem(payload.Item, mode)
			if err != nil {
				if mode == config.ModeStrict {
					return err
				}
				return nil
			}
			idx := state.nextIndex
			state.nextIndex++
			state.toolIndexByItemID[payload.Item.ID] = idx
			if payload.OutputIndex >= 0 {
				state.toolIndexByOutput[payload.OutputIndex] = idx
			}
			state.toolUsed = true
			start := types.AnthropicStreamContentBlockStart{
				Type:         "content_block_start",
				Index:        idx,
				ContentBlock: block,
			}
			return sendEvent(writer, "content_block_start", start)
		case "mcp_call":
			idx := state.nextIndex
			state.nextIndex++
			state.toolIndexByItemID[payload.Item.ID] = idx
			if payload.OutputIndex >= 0 {
				state.toolIndexByOutput[payload.OutputIndex] = idx
			}
			state.toolUsed = true
			block := types.AnthropicContentBlock{
				Type:          "tool_use",
				ID:            firstNonEmpty(payload.Item.CallID, payload.Item.ID),
				Name:          firstNonEmpty(payload.Item.Name, "MCP"),
				Input:         mustMarshalRaw(map[string]any{}),
				ResponsesType: "mcp_call",
			}
			start := types.AnthropicStreamContentBlockStart{
				Type:         "content_block_start",
				Index:        idx,
				ContentBlock: block,
			}
			return sendEvent(writer, "content_block_start", start)
		case "reasoning":
			// 记录但不立即创建 block（等 delta 到来时创建）
			return nil
		case "message", "tool_search_output", "image_generation_call", "computer_call_output":
			return nil
		case "web_search_call":
			// added 事件中没有 action 字段，只注册，不发射
			ws := &webSearchInfo{toolUseID: "srvtoolu_" + payload.Item.ID}
			state.webSearchByItemID[payload.Item.ID] = ws
			if payload.OutputIndex >= 0 {
				state.webSearchByOutIdx[payload.OutputIndex] = ws
			}
			return nil
		case "compaction", "compaction_summary":
			// 服务端 compaction → 静默跳过
			return nil
		default:
			if mode == config.ModeStrict {
				return fmt.Errorf("unsupported output item: %s", payload.Item.Type)
			}
		}
		return nil
	case "response.content_part.added":
		var payload struct {
			ItemID       string                     `json:"item_id"`
			OutputIndex  int                        `json:"output_index"`
			ContentIndex int                        `json:"content_index"`
			Part         types.OpenAIMessageContent `json:"part"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return err
		}
		if payload.Part.Type != "output_text" && payload.Part.Type != "text" {
			return nil
		}
		if err := ensureMessageStart(state, writer); err != nil {
			return err
		}
		key := contentKey(payload.ItemID, payload.OutputIndex, payload.ContentIndex)
		if _, ok := state.textIndexByKey[key]; ok {
			return nil
		}
		idx := state.nextIndex
		state.nextIndex++
		state.textIndexByKey[key] = idx
		state.textValueByKey[key] = ""
		return sendTextBlockStart(writer, idx)
	case "response.output_text.delta":
		var payload struct {
			ItemID       string `json:"item_id"`
			OutputIndex  int    `json:"output_index"`
			ContentIndex int    `json:"content_index"`
			Delta        string `json:"delta"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			log.Printf("ERROR: Failed to parse output_text.delta: %v, data=%s", err, truncate(event.Data, 200))
			return err
		}
		if err := ensureMessageStart(state, writer); err != nil {
			return err
		}
		key := contentKey(payload.ItemID, payload.OutputIndex, payload.ContentIndex)
		idx, ok := state.textIndexByKey[key]
		if !ok {
			idx = state.nextIndex
			state.nextIndex++
			state.textIndexByKey[key] = idx
			state.textValueByKey[key] = ""
			if err := sendTextBlockStart(writer, idx); err != nil {
				return err
			}
		}
		state.textValueByKey[key] += payload.Delta
		delta := types.AnthropicStreamContentBlockDelta{
			Type:  "content_block_delta",
			Index: idx,
			Delta: map[string]any{"type": "text_delta", "text": payload.Delta},
		}
		return sendEvent(writer, "content_block_delta", delta)
	case "response.output_text.done":
		// 等待 response.content_part.done，以便在关闭前补发 citations_delta。
		return nil
	case "response.function_call_arguments.delta":
		var payload struct {
			ItemID      string `json:"item_id"`
			OutputIndex int    `json:"output_index"`
			Delta       string `json:"delta"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return err
		}
		idx, ok := state.toolIndexByItemID[payload.ItemID]
		if !ok {
			idx, ok = state.toolIndexByOutput[payload.OutputIndex]
		}
		if !ok {
			if mode == config.ModeStrict {
				return errors.New("tool arguments delta without tool_use start")
			}
			return nil
		}
		delta := types.AnthropicStreamContentBlockDelta{
			Type:  "content_block_delta",
			Index: idx,
			Delta: map[string]any{"type": "input_json_delta", "partial_json": payload.Delta},
		}
		return sendEvent(writer, "content_block_delta", delta)
	case "response.mcp_call_arguments.delta":
		var payload struct {
			ItemID      string `json:"item_id"`
			OutputIndex int    `json:"output_index"`
			Delta       string `json:"delta"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return err
		}
		idx, ok := state.toolIndexByItemID[payload.ItemID]
		if !ok {
			idx, ok = state.toolIndexByOutput[payload.OutputIndex]
		}
		if !ok {
			if mode == config.ModeStrict {
				return errors.New("mcp arguments delta without tool_use start")
			}
			return nil
		}
		delta := types.AnthropicStreamContentBlockDelta{
			Type:  "content_block_delta",
			Index: idx,
			Delta: map[string]any{"type": "input_json_delta", "partial_json": payload.Delta},
		}
		return sendEvent(writer, "content_block_delta", delta)
	case "response.function_call_arguments.done":
		var payload struct {
			ItemID      string `json:"item_id"`
			OutputIndex int    `json:"output_index"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return err
		}
		idx, ok := state.toolIndexByItemID[payload.ItemID]
		if !ok {
			idx, ok = state.toolIndexByOutput[payload.OutputIndex]
		}
		if !ok {
			return nil
		}
		return closeContentBlock(writer, state, idx)
	case "response.mcp_call_arguments.done":
		var payload struct {
			ItemID      string `json:"item_id"`
			OutputIndex int    `json:"output_index"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return err
		}
		idx, ok := state.toolIndexByItemID[payload.ItemID]
		if !ok {
			idx, ok = state.toolIndexByOutput[payload.OutputIndex]
		}
		if !ok {
			return nil
		}
		return closeContentBlock(writer, state, idx)
	case "response.output_item.done":
		var payload struct {
			Item types.OpenAIOutputItem `json:"item"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return err
		}
		switch payload.Item.Type {
		case "function_call", "custom_tool_call":
			idx, ok := state.toolIndexByItemID[payload.Item.ID]
			if ok {
				return closeContentBlock(writer, state, idx)
			}
			return emitFunctionLikeToolUseFromDone(state, writer, payload.Item, mode)
		case "mcp_call":
			idx, ok := state.toolIndexByItemID[payload.Item.ID]
			if ok {
				if err := closeContentBlock(writer, state, idx); err != nil {
					return err
				}
				return handleMcpCallDone(state, writer, payload.Item, mode)
			}
			return emitFunctionLikeToolUseFromDone(state, writer, payload.Item, mode)
		case "local_shell_call", "tool_search_call", "computer_call", "mcp_list_tools":
			idx, ok := state.toolIndexByItemID[payload.Item.ID]
			if ok {
				return closeContentBlock(writer, state, idx)
			}
			return emitFunctionLikeToolUseFromDone(state, writer, payload.Item, mode)
		case "file_search_call":
			idx, ok := state.toolIndexByItemID[payload.Item.ID]
			if ok {
				if err := closeContentBlock(writer, state, idx); err != nil {
					return err
				}
				return handleFileSearchDone(state, writer, payload.Item, mode)
			}
			return emitFunctionLikeToolUseFromDone(state, writer, payload.Item, mode)
		case "reasoning":
			// reasoning 完毕 → 发送 signature_delta 然后关闭 thinking block
			if state.thinkingStarted && state.thinkingIndex >= 0 {
				return closeThinkingBlock(writer, state)
			}
			return emitReasoningFromDone(state, writer, payload.Item)
		case "message":
			if hasStreamedMessageItem(state, payload.Item.ID) {
				return nil
			}
			return emitMessageFromDone(state, writer, payload.Item, mode)
		case "web_search_call":
			return handleWebSearchDone(state, writer, payload.Item)
		case "tool_search_output":
			return handleToolSearchOutputDone(state, writer, payload.Item, mode)
		case "computer_call_output":
			return handleComputerCallOutputDone(state, writer, payload.Item, mode)
		case "image_generation_call":
			return emitImageGenerationFromDone(state, writer, payload.Item)
		case "compaction", "compaction_summary":
			return handleCompactionDone(state, writer, payload.Item)
		default:
			return emitOpaqueItemFromDone(state, writer, payload.Item, mode)
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		// 推理摘要增量 或 推理原文增量 → thinking_delta
		var payload struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return err
		}
		if err := ensureMessageStart(state, writer); err != nil {
			return err
		}
		// 首次收到 reasoning delta → 创建 thinking content_block
		if !state.thinkingStarted {
			if err := startThinkingBlock(state, writer); err != nil {
				return err
			}
		}
		delta := types.AnthropicStreamContentBlockDelta{
			Type:  "content_block_delta",
			Index: state.thinkingIndex,
			Delta: map[string]any{"type": "thinking_delta", "thinking": payload.Delta},
		}
		return sendEvent(writer, "content_block_delta", delta)
	case "response.reasoning_summary_text.done", "response.reasoning_text.done":
		// 推理文本完毕 → 发送 signature_delta 然后关闭 thinking block
		if state.thinkingStarted && state.thinkingIndex >= 0 {
			return closeThinkingBlock(writer, state)
		}
		return nil
	case "response.reasoning_summary_part.added",
		"response.reasoning_summary_part.done",
		"response.reasoning_summary.delta",
		"response.reasoning_summary.done":
		// summary part 生命周期事件 → 忽略（由 text delta 驱动）
		return nil
	case "response.content_part.done":
		// 安全网：如果有未关闭的 text block，通过 content_part.done 关闭
		var payload struct {
			ItemID       string                     `json:"item_id"`
			OutputIndex  int                        `json:"output_index"`
			ContentIndex int                        `json:"content_index"`
			Part         types.OpenAIMessageContent `json:"part"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil
		}
		key := contentKey(payload.ItemID, payload.OutputIndex, payload.ContentIndex)
		idx, ok := state.textIndexByKey[key]
		if ok {
			text := payload.Part.Text
			if strings.TrimSpace(text) == "" {
				text = state.textValueByKey[key]
			}
			if len(payload.Part.Annotations) > 0 {
				if err := emitCitationsDelta(writer, idx, payload.Part.Annotations, text); err != nil {
					return err
				}
			}
			return closeContentBlock(writer, state, idx)
		}
		return nil
	case "response.refusal.delta":
		// refusal delta → 转为 text_delta
		var payload struct {
			ItemID       string `json:"item_id"`
			OutputIndex  int    `json:"output_index"`
			ContentIndex int    `json:"content_index"`
			Delta        string `json:"delta"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return err
		}
		if err := ensureMessageStart(state, writer); err != nil {
			return err
		}
		key := contentKey(payload.ItemID, payload.OutputIndex, payload.ContentIndex)
		idx, ok := state.textIndexByKey[key]
		if !ok {
			idx = state.nextIndex
			state.nextIndex++
			state.textIndexByKey[key] = idx
			state.textValueByKey[key] = ""
			if err := sendTypedTextBlockStart(writer, idx, "refusal"); err != nil {
				return err
			}
		}
		state.textValueByKey[key] += payload.Delta
		delta := types.AnthropicStreamContentBlockDelta{
			Type:  "content_block_delta",
			Index: idx,
			Delta: map[string]any{"type": "text_delta", "text": payload.Delta},
		}
		return sendEvent(writer, "content_block_delta", delta)
	case "response.refusal.done":
		// refusal 完毕 → 关闭对应的 text block
		var payload struct {
			ItemID       string `json:"item_id"`
			OutputIndex  int    `json:"output_index"`
			ContentIndex int    `json:"content_index"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil
		}
		key := contentKey(payload.ItemID, payload.OutputIndex, payload.ContentIndex)
		idx, ok := state.textIndexByKey[key]
		if ok {
			return closeContentBlock(writer, state, idx)
		}
		return nil
	case "response.web_search_call.completed":
		if err := ensureMessageStart(state, writer); err != nil {
			return err
		}
		return handleWebSearchCompleted(state, writer, event.Data)
	case "response.web_search_call.in_progress",
		"response.web_search_call.searching",
		"response.file_search_call.in_progress",
		"response.file_search_call.searching",
		"response.file_search_call.completed",
		"response.computer_call.in_progress",
		"response.computer_call.completed",
		"response.mcp_call.in_progress",
		"response.mcp_call.completed",
		"response.mcp_call.failed",
		"response.mcp_list_tools.in_progress",
		"response.mcp_list_tools.completed":
		// 不支持的功能调用事件 → 静默忽略
		return nil
	case "response.completed", "response.incomplete", "response.cancelled", "response.failed":
		var payload struct {
			Response types.OpenAIResponse `json:"response"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return err
		}
		if err := ensureMessageStart(state, writer); err != nil {
			return err
		}
		if err := flushOpenTextBlocks(writer, state); err != nil {
			return err
		}
		stopReason := deriveStopReason(payload.Response, state.toolUsed)
		msgDelta := types.AnthropicStreamMessageDelta{
			Type: "message_delta",
			Delta: types.AnthropicMessageDelta{
				StopReason: stopReason,
			},
			ContextManagement: payload.Response.ContextManagement,
		}
		if payload.Response.Usage != nil {
			usage := &types.AnthropicUsage{
				InputTokens:  payload.Response.Usage.InputTokens,
				OutputTokens: payload.Response.Usage.OutputTokens,
				ServiceTier:  state.serviceTier,
				Speed:        state.speed,
			}
			if payload.Response.Usage.InputTokenDetails != nil && payload.Response.Usage.InputTokenDetails.CachedTokens > 0 {
				usage.CacheReadInputTokens = payload.Response.Usage.InputTokenDetails.CachedTokens
				backfillAnthropicCacheCreation(usage, payload.Response.Usage.InputTokenDetails.CachedTokens, state.promptCacheRetention)
			}
			if usage.InputTokens > 0 || usage.OutputTokens > 0 {
				usage.Iterations = []types.AnthropicUsageIteration{{
					InputTokens:  usage.InputTokens,
					OutputTokens: usage.OutputTokens,
				}}
			}
			if state.serverToolUseCount > 0 {
				usage.ServerToolUse = &types.AnthropicServerToolUse{
					WebSearchRequests: state.serverToolUseCount,
				}
			}
			msgDelta.Usage = usage
		} else if state.serverToolUseCount > 0 {
			msgDelta.Usage = &types.AnthropicUsage{
				ServiceTier: state.serviceTier,
				Speed:       state.speed,
				ServerToolUse: &types.AnthropicServerToolUse{
					WebSearchRequests: state.serverToolUseCount,
				},
			}
		}
		if err := sendEvent(writer, "message_delta", msgDelta); err != nil {
			return err
		}
		stop := types.AnthropicStreamMessageStop{Type: "message_stop"}
		return sendEvent(writer, "message_stop", stop)
	case "error":
		var errPayload struct {
			Error struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(event.Data, &errPayload); err != nil {
			return fmt.Errorf("upstream error event")
		}
		return fmt.Errorf("%s: %s", errPayload.Error.Type, errPayload.Error.Message)
	default:
		log.Printf("WARN: Unhandled event type: %s, data=%s", event.Name, truncate(event.Data, 200))
		return nil
	}
}

func ensureMessageStart(state *streamState, writer *sse.Writer) error {
	if state.started {
		return nil
	}
	if state.messageID == "" {
		state.messageID = "msg_stream"
	}
	msg := types.AnthropicMessageResponse{
		ID:      state.messageID,
		Type:    "message",
		Role:    "assistant",
		Content: []types.AnthropicContentBlock{},
		Model:   state.model,
		Usage: types.AnthropicUsage{
			InputTokens: state.estimatedInputTokens,
			ServiceTier: state.serviceTier,
			Speed:       state.speed,
		},
	}
	start := types.AnthropicStreamMessageStart{
		Type:    "message_start",
		Message: msg,
	}
	state.started = true
	return sendEvent(writer, "message_start", start)
}

func closeContentBlock(writer *sse.Writer, state *streamState, index int) error {
	if state.closedIndex[index] {
		return nil
	}
	state.closedIndex[index] = true
	stop := types.AnthropicStreamContentBlockStop{Type: "content_block_stop", Index: index}
	return sendEvent(writer, "content_block_stop", stop)
}

func emitCitationsDelta(writer *sse.Writer, index int, annotations []types.OpenAIAnnotation, text string) error {
	citations := annotationsToCitations(annotations, text)
	if len(citations) == 0 {
		return nil
	}
	var parsed []map[string]any
	if err := json.Unmarshal(citations, &parsed); err != nil {
		return nil
	}
	for _, citation := range parsed {
		delta := types.AnthropicStreamContentBlockDelta{
			Type:  "content_block_delta",
			Index: index,
			Delta: map[string]any{
				"type":     "citations_delta",
				"citation": citation,
			},
		}
		if err := sendEvent(writer, "content_block_delta", delta); err != nil {
			return err
		}
	}
	return nil
}

func flushOpenTextBlocks(writer *sse.Writer, state *streamState) error {
	indices := make([]int, 0, len(state.textIndexByKey))
	for _, idx := range state.textIndexByKey {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	for _, idx := range indices {
		if err := closeContentBlock(writer, state, idx); err != nil {
			return err
		}
	}
	if state.thinkingStarted && state.thinkingIndex >= 0 && !state.closedIndex[state.thinkingIndex] {
		if err := closeThinkingBlock(writer, state); err != nil {
			return err
		}
	}
	return nil
}

// startThinkingBlock 创建 thinking content_block_start 事件
// 使用 map 构造 JSON 以确保 "thinking": "" 始终存在（避免 omitempty 丢失）
func startThinkingBlock(state *streamState, writer *sse.Writer) error {
	idx := state.nextIndex
	state.nextIndex++
	state.thinkingIndex = idx
	state.thinkingStarted = true

	startData := map[string]any{
		"type":  "content_block_start",
		"index": idx,
		"content_block": map[string]any{
			"type":     "thinking",
			"thinking": "",
		},
	}
	data, err := json.Marshal(startData)
	if err != nil {
		return err
	}
	return writer.Send("content_block_start", data)
}

// closeThinkingBlock 发送 signature_delta 然后关闭 thinking block
// signature 占位符让 Claude Code CLI 将 thinking 渲染为折叠 UI
func closeThinkingBlock(writer *sse.Writer, state *streamState) error {
	if state.closedIndex[state.thinkingIndex] {
		return nil
	}
	// 发送 signature_delta
	sigDelta := map[string]any{
		"type":  "content_block_delta",
		"index": state.thinkingIndex,
		"delta": map[string]any{
			"type":      "signature_delta",
			"signature": "proxy-bridge-signature-placeholder",
		},
	}
	sigData, err := json.Marshal(sigDelta)
	if err != nil {
		return err
	}
	if err := writer.Send("content_block_delta", sigData); err != nil {
		return err
	}
	return closeContentBlock(writer, state, state.thinkingIndex)
}

// sendTextBlockStart 发送 text 类型的 content_block_start 事件
// 使用 map 构造 JSON 以确保 "text": "" 始终存在（避免 omitempty 丢失）
func sendTextBlockStart(writer *sse.Writer, idx int) error {
	return sendTypedTextBlockStart(writer, idx, "")
}

func sendTypedTextBlockStart(writer *sse.Writer, idx int, responsesType string) error {
	contentBlock := map[string]any{
		"type": "text",
		"text": "",
	}
	if strings.TrimSpace(responsesType) != "" {
		contentBlock["responses_type"] = responsesType
	}
	startData := map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": contentBlock,
	}
	data, err := json.Marshal(startData)
	if err != nil {
		return err
	}
	return writer.Send("content_block_start", data)
}

func sendEvent(writer *sse.Writer, name string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return writer.Send(name, data)
}

func contentKey(itemID string, outputIndex, contentIndex int) string {
	if itemID != "" {
		return fmt.Sprintf("%s:%d", itemID, contentIndex)
	}
	return fmt.Sprintf("out:%d:%d", outputIndex, contentIndex)
}

func truncate(data []byte, maxLen int) string {
	if len(data) <= maxLen {
		return string(data)
	}
	return string(data[:maxLen]) + "..."
}

// handleWebSearchCompleted 处理搜索完成事件（由 output_item.done 统一处理，此处空实现）
func handleWebSearchCompleted(state *streamState, writer *sse.Writer, eventData []byte) error {
	return nil
}

// handleWebSearchDone 处理 web_search_call 完成 → 发射 server_tool_use + web_search_tool_result
// output_item.done 是唯一携带完整 action（含 query）的事件
func handleWebSearchDone(state *streamState, writer *sse.Writer, item types.OpenAIOutputItem) error {
	ws := state.webSearchByItemID[item.ID]
	if ws == nil {
		ws = &webSearchInfo{toolUseID: "srvtoolu_" + item.ID}
		state.webSearchByItemID[item.ID] = ws
	}
	if ws.resultEmitted {
		return nil
	}
	ws.resultEmitted = true
	state.serverToolUseCount++

	// 解析搜索查询
	var action struct {
		Query string `json:"query"`
	}
	if len(item.Action) > 0 {
		_ = json.Unmarshal(item.Action, &action)
	}

	// 1. 发射 server_tool_use（先 start 带空 input，再用 input_json_delta 发送查询）
	idx1 := state.nextIndex
	state.nextIndex++
	startData := map[string]any{
		"type":  "content_block_start",
		"index": idx1,
		"content_block": map[string]any{
			"type":  "server_tool_use",
			"id":    ws.toolUseID,
			"name":  "web_search",
			"input": map[string]any{},
		},
	}
	data, _ := json.Marshal(startData)
	if err := writer.Send("content_block_start", data); err != nil {
		return err
	}
	// 发送 input_json_delta（匹配 Anthropic 原生流式行为）
	inputJSON, _ := json.Marshal(map[string]string{"query": action.Query})
	inputDelta := map[string]any{
		"type":  "content_block_delta",
		"index": idx1,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": string(inputJSON),
		},
	}
	deltaData, _ := json.Marshal(inputDelta)
	if err := writer.Send("content_block_delta", deltaData); err != nil {
		return err
	}
	if err := closeContentBlock(writer, state, idx1); err != nil {
		return err
	}

	// 2. 发射 web_search_tool_result
	return emitWebSearchResult(state, writer, ws)
}

// emitWebSearchResult 发射 web_search_tool_result content_block
func emitWebSearchResult(state *streamState, writer *sse.Writer, ws *webSearchInfo) error {
	idx := state.nextIndex
	state.nextIndex++

	startData := map[string]any{
		"type":  "content_block_start",
		"index": idx,
		"content_block": map[string]any{
			"type":        "web_search_tool_result",
			"tool_use_id": ws.toolUseID,
			"content":     []any{},
		},
	}
	data, _ := json.Marshal(startData)
	if err := writer.Send("content_block_start", data); err != nil {
		return err
	}
	return closeContentBlock(writer, state, idx)
}

func handleToolSearchOutputDone(state *streamState, writer *sse.Writer, item types.OpenAIOutputItem, mode config.Mode) error {
	block, err := openAIToolSearchOutputToBlock(item, mode)
	if err != nil {
		return err
	}
	if block == nil {
		return nil
	}
	return emitFullContentBlock(state, writer, *block)
}

func handleFileSearchDone(state *streamState, writer *sse.Writer, item types.OpenAIOutputItem, mode config.Mode) error {
	block, err := openAIFileSearchResultToBlock(item, mode)
	if err != nil {
		return err
	}
	if block == nil {
		return nil
	}
	return emitFullContentBlock(state, writer, *block)
}

func handleCompactionDone(state *streamState, writer *sse.Writer, item types.OpenAIOutputItem) error {
	if strings.TrimSpace(item.EncryptedContent) == "" {
		return nil
	}
	idx := state.nextIndex
	state.nextIndex++

	startData := map[string]any{
		"type":  "content_block_start",
		"index": idx,
		"content_block": map[string]any{
			"type":    "compaction",
			"content": item.EncryptedContent,
		},
	}
	data, err := json.Marshal(startData)
	if err != nil {
		return err
	}
	if err := writer.Send("content_block_start", data); err != nil {
		return err
	}
	return closeContentBlock(writer, state, idx)
}

func handleComputerCallOutputDone(state *streamState, writer *sse.Writer, item types.OpenAIOutputItem, mode config.Mode) error {
	block, err := openAIComputerCallOutputToBlock(item, mode)
	if err != nil {
		return err
	}
	if block == nil {
		return nil
	}
	return emitFullContentBlock(state, writer, *block)
}

func handleMcpCallDone(state *streamState, writer *sse.Writer, item types.OpenAIOutputItem, mode config.Mode) error {
	block, err := openAIMcpCallResultToBlock(item, mode)
	if err != nil {
		return err
	}
	if block == nil {
		return nil
	}
	return emitFullContentBlock(state, writer, *block)
}

func toolUseBlockFromAddedItem(item types.OpenAIOutputItem, mode config.Mode) (types.AnthropicContentBlock, error) {
	switch item.Type {
	case "local_shell_call":
		return openAILocalShellCallToBlock(item, mode)
	case "tool_search_call":
		return openAIToolSearchCallToBlock(item, mode)
	case "file_search_call":
		return openAISpecialToolCallToBlock(item, mode, "FileSearch")
	case "computer_call":
		return openAISpecialToolCallToBlock(item, mode, "Computer")
	case "mcp_list_tools":
		return openAISpecialToolCallToBlock(item, mode, "MCPListTools")
	default:
		return types.AnthropicContentBlock{}, fmt.Errorf("unsupported added item: %s", item.Type)
	}
}

func emitFunctionLikeToolUseFromDone(state *streamState, writer *sse.Writer, item types.OpenAIOutputItem, mode config.Mode) error {
	var (
		blocks []types.AnthropicContentBlock
		err    error
	)
	switch item.Type {
	case "function_call":
		var block types.AnthropicContentBlock
		block, err = openAIFunctionCallToBlock(item, mode)
		blocks = []types.AnthropicContentBlock{block}
	case "custom_tool_call":
		var block types.AnthropicContentBlock
		block, err = openAICustomToolCallToBlock(item, mode)
		blocks = []types.AnthropicContentBlock{block}
	case "local_shell_call":
		var block types.AnthropicContentBlock
		block, err = openAILocalShellCallToBlock(item, mode)
		blocks = []types.AnthropicContentBlock{block}
	case "tool_search_call":
		var block types.AnthropicContentBlock
		block, err = openAIToolSearchCallToBlock(item, mode)
		blocks = []types.AnthropicContentBlock{block}
	case "file_search_call":
		blocks, err = openAIFileSearchCallToBlocks(item, mode)
	case "computer_call":
		var block types.AnthropicContentBlock
		block, err = openAISpecialToolCallToBlock(item, mode, "Computer")
		blocks = []types.AnthropicContentBlock{block}
	case "mcp_call":
		blocks, err = openAIMcpCallToBlocks(item, mode)
	case "mcp_list_tools":
		var block types.AnthropicContentBlock
		block, err = openAISpecialToolCallToBlock(item, mode, "MCPListTools")
		blocks = []types.AnthropicContentBlock{block}
	default:
		return nil
	}
	if err != nil {
		return err
	}
	state.toolUsed = true
	for _, block := range blocks {
		if err := emitFullContentBlock(state, writer, block); err != nil {
			return err
		}
	}
	return nil
}

func emitMessageFromDone(state *streamState, writer *sse.Writer, item types.OpenAIOutputItem, mode config.Mode) error {
	blocks, err := openAIMessageToBlocks(item, mode)
	if err != nil {
		return err
	}
	for _, block := range blocks {
		if err := emitFullContentBlock(state, writer, block); err != nil {
			return err
		}
	}
	return nil
}

func emitReasoningFromDone(state *streamState, writer *sse.Writer, item types.OpenAIOutputItem) error {
	block, err := openAIReasoningToThinkingBlock(item)
	if err != nil || block == nil {
		return err
	}
	return emitFullContentBlock(state, writer, *block)
}

func emitImageGenerationFromDone(state *streamState, writer *sse.Writer, item types.OpenAIOutputItem) error {
	return emitFullContentBlock(state, writer, openAIImageGenerationToBlock(item))
}

func emitOpaqueItemFromDone(state *streamState, writer *sse.Writer, item types.OpenAIOutputItem, mode config.Mode) error {
	block, err := openAIOpaqueOutputToBlock(item)
	if err != nil {
		if mode == config.ModeStrict {
			return err
		}
		return nil
	}
	return emitFullContentBlock(state, writer, *block)
}

func emitFullContentBlock(state *streamState, writer *sse.Writer, block types.AnthropicContentBlock) error {
	if err := ensureMessageStart(state, writer); err != nil {
		return err
	}
	idx := state.nextIndex
	state.nextIndex++
	start := types.AnthropicStreamContentBlockStart{
		Type:         "content_block_start",
		Index:        idx,
		ContentBlock: block,
	}
	if err := sendEvent(writer, "content_block_start", start); err != nil {
		return err
	}
	return closeContentBlock(writer, state, idx)
}

func hasStreamedMessageItem(state *streamState, itemID string) bool {
	if strings.TrimSpace(itemID) == "" {
		return false
	}
	prefix := itemID + ":"
	for key := range state.textIndexByKey {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func anthropicServiceTier(serviceTier string) string {
	switch strings.TrimSpace(strings.ToLower(serviceTier)) {
	case "":
		return "standard"
	case "priority":
		return "priority"
	case "flex":
		return "flex"
	default:
		return strings.TrimSpace(strings.ToLower(serviceTier))
	}
}

func anthropicSpeed(serviceTier string) string {
	switch strings.TrimSpace(strings.ToLower(serviceTier)) {
	case "priority":
		return "fast"
	default:
		return "standard"
	}
}

func backfillAnthropicCacheCreation(usage *types.AnthropicUsage, cachedTokens int, retention string) {
	if usage == nil || cachedTokens <= 0 {
		return
	}
	if usage.CacheCreation == nil {
		usage.CacheCreation = &types.AnthropicCacheCreation{}
	}
	switch strings.TrimSpace(strings.ToLower(retention)) {
	case "", "in_memory", "5m":
		usage.CacheCreation.Ephemeral5MInputTokens = cachedTokens
	default:
		usage.CacheCreation.Ephemeral1HInputTokens = cachedTokens
	}
}
