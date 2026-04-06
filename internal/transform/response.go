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

const storedResponsesItemExtraKey = "responses_item"

func TransformOpenAIToAnthropic(resp types.OpenAIResponse, mode config.Mode, requestModel string) (types.AnthropicMessageResponse, error) {
	content := make([]types.AnthropicContentBlock, 0, len(resp.Output))
	// thinking 块收集（放在所有 content 之前）
	var thinkingBlocks []types.AnthropicContentBlock
	toolUsed := false
	serverToolUseCount := 0

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
		case "local_shell_call":
			block, err := openAILocalShellCallToBlock(item, mode)
			if err != nil {
				return types.AnthropicMessageResponse{}, err
			}
			content = append(content, block)
			toolUsed = true
		case "tool_search_call":
			block, err := openAIToolSearchCallToBlock(item, mode)
			if err != nil {
				return types.AnthropicMessageResponse{}, err
			}
			content = append(content, block)
			toolUsed = true
		case "tool_search_output":
			block, err := openAIToolSearchOutputToBlock(item, mode)
			if err != nil {
				return types.AnthropicMessageResponse{}, err
			}
			if block != nil {
				content = append(content, *block)
			}
		case "custom_tool_call":
			block, err := openAICustomToolCallToBlock(item, mode)
			if err != nil {
				return types.AnthropicMessageResponse{}, err
			}
			content = append(content, block)
			toolUsed = true
		case "file_search_call":
			blocks, err := openAIFileSearchCallToBlocks(item, mode)
			if err != nil {
				return types.AnthropicMessageResponse{}, err
			}
			content = append(content, blocks...)
			toolUsed = true
		case "computer_call":
			block, err := openAISpecialToolCallToBlock(item, mode, "Computer")
			if err != nil {
				return types.AnthropicMessageResponse{}, err
			}
			content = append(content, block)
			toolUsed = true
		case "mcp_call":
			blocks, err := openAIMcpCallToBlocks(item, mode)
			if err != nil {
				return types.AnthropicMessageResponse{}, err
			}
			content = append(content, blocks...)
			toolUsed = true
		case "mcp_list_tools":
			block, err := openAISpecialToolCallToBlock(item, mode, "MCPListTools")
			if err != nil {
				return types.AnthropicMessageResponse{}, err
			}
			content = append(content, block)
		case "computer_call_output":
			block, err := openAIComputerCallOutputToBlock(item, mode)
			if err != nil {
				return types.AnthropicMessageResponse{}, err
			}
			if block != nil {
				content = append(content, *block)
			}
		case "web_search_call":
			blocks := openAIWebSearchToBlocks(item, allAnnotations)
			content = append(content, blocks...)
			serverToolUseCount++
		case "compaction", "compaction_summary":
			block, err := openAICompactionToBlock(item, mode)
			if err != nil {
				if mode == config.ModeStrict {
					return types.AnthropicMessageResponse{}, err
				}
				log.Printf("WARN: skipping compaction block: %v", err)
				continue
			}
			if block != nil {
				content = append(content, *block)
			}
		case "image_generation_call":
			content = append(content, openAIImageGenerationToBlock(item))
		default:
			block, err := openAIOpaqueOutputToBlock(item)
			if err != nil {
				if mode == config.ModeStrict {
					return types.AnthropicMessageResponse{}, fmt.Errorf("unsupported output item type: %s", item.Type)
				}
				log.Printf("WARN: converting unsupported output type to text: %s", item.Type)
				content = append(content, types.AnthropicContentBlock{
					Type: "text",
					Text: fmt.Sprintf("[unsupported output item: %s]", item.Type),
				})
				continue
			}
			content = append(content, *block)
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
		if resp.Usage.InputTokenDetails != nil && resp.Usage.InputTokenDetails.CachedTokens > 0 {
			usage.CacheReadInputTokens = resp.Usage.InputTokenDetails.CachedTokens
		}
	}
	if serverToolUseCount > 0 {
		usage.ServerToolUse = &types.AnthropicServerToolUse{
			WebSearchRequests: serverToolUseCount,
		}
	}

	return types.AnthropicMessageResponse{
		ID:                "msg_" + resp.ID,
		Type:              "message",
		Role:              "assistant",
		Content:           content,
		Model:             requestModel,
		StopReason:        stopReason,
		Usage:             usage,
		ContextManagement: resp.ContextManagement,
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
			block := types.AnthropicContentBlock{
				Type:    "text",
				Text:    part.Text,
				Phase:   item.Phase,
				EndTurn: item.EndTurn,
			}
			if len(part.Annotations) > 0 {
				block.Citations = annotationsToCitations(part.Annotations, part.Text)
			}
			blocks = append(blocks, block)
		case "refusal":
			text := strings.TrimSpace(part.Refusal)
			if text == "" {
				text = "[refusal]"
			}
			blocks = append(blocks, types.AnthropicContentBlock{
				Type:          "text",
				Text:          text,
				Phase:         item.Phase,
				EndTurn:       item.EndTurn,
				ResponsesType: "refusal",
			})
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
	args := openAIArgumentsString(item.Arguments)
	if args != "" {
		if err := json.Unmarshal([]byte(args), &input); err != nil {
			if mode == config.ModeStrict {
				return types.AnthropicContentBlock{}, err
			}
			input = map[string]any{"_raw": args}
		}
	}

	block := types.AnthropicContentBlock{
		Type:          "tool_use",
		ID:            item.CallID,
		Name:          item.Name,
		Namespace:     item.Namespace,
		Input:         mustMarshalRaw(input),
		ResponsesType: "function_call",
	}
	attachStoredResponseItem(&block, item)
	return block, nil
}

func openAILocalShellCallToBlock(item types.OpenAIOutputItem, mode config.Mode) (types.AnthropicContentBlock, error) {
	if item.CallID == "" {
		return types.AnthropicContentBlock{}, errors.New("local_shell_call missing call_id")
	}
	input := map[string]any{}
	if len(item.Action) > 0 {
		var action map[string]any
		if err := json.Unmarshal(item.Action, &action); err == nil {
			if commandList, ok := action["command"].([]any); ok && len(commandList) > 0 {
				var parts []string
				for _, entry := range commandList {
					if s, ok := entry.(string); ok {
						parts = append(parts, s)
					}
				}
				if len(parts) > 0 {
					input["command"] = strings.Join(parts, " ")
				}
			}
			for key, val := range action {
				if _, exists := input[key]; !exists {
					input[key] = val
				}
			}
		} else if mode == config.ModeStrict {
			return types.AnthropicContentBlock{}, fmt.Errorf("invalid local_shell_call action: %w", err)
		}
	}
	if _, ok := input["command"]; !ok {
		input["command"] = ""
	}
	if len(item.Action) > 0 {
		var action struct {
			Command          []string          `json:"command"`
			TimeoutMS        any               `json:"timeout_ms"`
			WorkingDirectory string            `json:"working_directory"`
			Env              map[string]string `json:"env"`
			User             string            `json:"user"`
		}
		if err := json.Unmarshal(item.Action, &action); err == nil {
			if action.TimeoutMS != nil {
				input["timeout"] = action.TimeoutMS
			}
			if strings.TrimSpace(action.WorkingDirectory) != "" {
				input["cwd"] = action.WorkingDirectory
			}
			if len(action.Env) > 0 {
				envAny := make(map[string]any, len(action.Env))
				for k, v := range action.Env {
					envAny[k] = v
				}
				input["env"] = envAny
			}
			if strings.TrimSpace(action.User) != "" {
				input["user"] = action.User
			}
			if len(action.Command) > 0 {
				input["command"] = unwrapShellCommand(action.Command)
			}
		}
	}
	if item.Status == "in_progress" {
		input["run_in_background"] = true
	}
	block := types.AnthropicContentBlock{
		Type:          "tool_use",
		ID:            item.CallID,
		Name:          "Bash",
		Input:         mustMarshalRaw(input),
		Status:        item.Status,
		Action:        item.Action,
		Execution:     item.Execution,
		ResponsesType: "local_shell_call",
	}
	attachStoredResponseItem(&block, item)
	return block, nil
}

func openAIToolSearchCallToBlock(item types.OpenAIOutputItem, mode config.Mode) (types.AnthropicContentBlock, error) {
	if item.CallID == "" {
		return types.AnthropicContentBlock{}, errors.New("tool_search_call missing call_id")
	}
	input := map[string]any{}
	if len(item.Arguments) > 0 {
		if err := json.Unmarshal(item.Arguments, &input); err != nil {
			if mode == config.ModeStrict {
				return types.AnthropicContentBlock{}, fmt.Errorf("invalid tool_search_call arguments: %w", err)
			}
			input = map[string]any{"_raw": string(item.Arguments)}
		}
	}
	block := types.AnthropicContentBlock{
		Type:          "tool_use",
		ID:            item.CallID,
		Name:          "ToolSearch",
		Input:         mustMarshalRaw(input),
		Status:        item.Status,
		Execution:     firstNonEmpty(item.Execution, "client"),
		ResponsesType: "tool_search_call",
	}
	attachStoredResponseItem(&block, item)
	return block, nil
}

func openAICustomToolCallToBlock(item types.OpenAIOutputItem, mode config.Mode) (types.AnthropicContentBlock, error) {
	if item.CallID == "" || item.Name == "" {
		return types.AnthropicContentBlock{}, errors.New("custom_tool_call missing call_id or name")
	}

	input := map[string]any{}
	rawInput := openAIArgumentsString(item.Input)
	if len(item.Input) > 0 {
		if err := json.Unmarshal(item.Input, &input); err != nil {
			input = map[string]any{"_raw": rawInput}
		}
	}

	block := types.AnthropicContentBlock{
		Type:          "tool_use",
		ID:            item.CallID,
		Name:          item.Name,
		Namespace:     item.Namespace,
		Input:         mustMarshalRaw(input),
		ResponsesType: "custom_tool_call",
		RawInput:      rawInput,
	}
	attachStoredResponseItem(&block, item)
	return block, nil
}

func openAIToolSearchOutputToBlock(item types.OpenAIOutputItem, mode config.Mode) (*types.AnthropicContentBlock, error) {
	toolUseID := item.CallID
	if toolUseID == "" {
		if mode == config.ModeStrict {
			return nil, errors.New("tool_search_output missing call_id")
		}
		return nil, nil
	}
	refs, err := toolSearchToolsToReferences(item.Tools, mode)
	if err != nil {
		return nil, err
	}
	block := &types.AnthropicContentBlock{
		Type:      "tool_search_tool_result",
		ToolUseID: toolUseID,
		Content:   mustMarshalRaw(refs),
		Tools:     append(json.RawMessage(nil), item.Tools...),
		Status:    item.Status,
		Execution: item.Execution,
	}
	attachStoredResponseItem(block, item)
	return block, nil
}

func openAISpecialToolCallToBlock(item types.OpenAIOutputItem, mode config.Mode, fallbackName string) (types.AnthropicContentBlock, error) {
	blockID := firstNonEmpty(item.CallID, item.ID)
	if blockID == "" {
		return types.AnthropicContentBlock{}, fmt.Errorf("%s missing id/call_id", item.Type)
	}

	input := map[string]any{}
	rawInput := ""

	switch {
	case len(item.Arguments) > 0:
		rawInput = openAIArgumentsString(item.Arguments)
		if err := parseToolLikeInputJSON(rawInput, &input); err != nil {
			if mode == config.ModeStrict {
				return types.AnthropicContentBlock{}, fmt.Errorf("invalid %s arguments: %w", item.Type, err)
			}
			input = map[string]any{"_raw": rawInput}
		}
	case len(item.Input) > 0:
		rawInput = openAIArgumentsString(item.Input)
		if err := json.Unmarshal(item.Input, &input); err != nil {
			if mode == config.ModeStrict {
				return types.AnthropicContentBlock{}, fmt.Errorf("invalid %s input: %w", item.Type, err)
			}
			input = map[string]any{"_raw": rawInput}
		}
	case len(item.Action) > 0:
		rawInput = strings.TrimSpace(string(item.Action))
		if err := json.Unmarshal(item.Action, &input); err != nil {
			if mode == config.ModeStrict {
				return types.AnthropicContentBlock{}, fmt.Errorf("invalid %s action: %w", item.Type, err)
			}
			input = map[string]any{"_raw_action": rawInput}
		}
	}

	if serverLabel := firstRawString(item.ExtraFields, "server_label"); serverLabel != "" {
		if _, ok := input["server_label"]; !ok {
			input["server_label"] = serverLabel
		}
	}
	if len(item.Queries) > 0 {
		if _, ok := input["queries"]; !ok {
			input["queries"] = append([]string(nil), item.Queries...)
		}
	}
	if len(item.Results) > 0 {
		var parsed any
		if err := json.Unmarshal(item.Results, &parsed); err == nil {
			if _, ok := input["results"]; !ok {
				input["results"] = parsed
			}
		}
	}
	if len(item.Tools) > 0 {
		var parsedTools []any
		if err := json.Unmarshal(item.Tools, &parsedTools); err == nil {
			input["tool_count"] = len(parsedTools)
		}
	}
	if len(item.Output) > 0 {
		var parsed any
		if err := json.Unmarshal(item.Output, &parsed); err == nil {
			input["output"] = parsed
		} else {
			input["output"] = strings.TrimSpace(string(item.Output))
		}
	}
	if len(item.Error) > 0 {
		var parsed any
		if err := json.Unmarshal(item.Error, &parsed); err == nil {
			input["error"] = parsed
		} else {
			input["error"] = strings.TrimSpace(string(item.Error))
		}
	}
	if len(item.AcknowledgedSafetyChecks) > 0 {
		var parsed any
		if err := json.Unmarshal(item.AcknowledgedSafetyChecks, &parsed); err == nil {
			input["acknowledged_safety_checks"] = parsed
		}
	}
	if len(input) == 0 {
		input = map[string]any{}
	}

	block := types.AnthropicContentBlock{
		Type:          "tool_use",
		ID:            blockID,
		Name:          firstNonEmpty(item.Name, fallbackName),
		Namespace:     item.Namespace,
		Input:         mustMarshalRaw(input),
		Status:        item.Status,
		Execution:     item.Execution,
		Action:        cloneRaw(item.Action),
		Tools:         cloneRaw(item.Tools),
		ResponsesType: item.Type,
		RawInput:      rawInput,
	}
	attachStoredResponseItem(&block, item)
	return block, nil
}

func openAIFileSearchCallToBlocks(item types.OpenAIOutputItem, mode config.Mode) ([]types.AnthropicContentBlock, error) {
	callBlock, err := openAISpecialToolCallToBlock(item, mode, "FileSearch")
	if err != nil {
		return nil, err
	}
	blocks := []types.AnthropicContentBlock{callBlock}
	resultBlock, err := openAIFileSearchResultToBlock(item, mode)
	if err != nil {
		return nil, err
	}
	if resultBlock != nil {
		blocks = append(blocks, *resultBlock)
	}
	return blocks, nil
}

func openAIFileSearchResultToBlock(item types.OpenAIOutputItem, mode config.Mode) (*types.AnthropicContentBlock, error) {
	if len(item.Results) == 0 {
		return nil, nil
	}
	results, err := fileSearchResultsToBlocks(item.Results, mode)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	block := &types.AnthropicContentBlock{
		Type:      "tool_result",
		ToolUseID: firstNonEmpty(item.CallID, item.ID),
		Content:   mustMarshalRaw(results),
		Status:    item.Status,
	}
	attachStoredResponseItem(block, item)
	return block, nil
}

func fileSearchResultsToBlocks(raw json.RawMessage, mode config.Mode) ([]types.AnthropicContentBlock, error) {
	var results []map[string]any
	if err := json.Unmarshal(raw, &results); err != nil {
		if mode == config.ModeStrict {
			return nil, err
		}
		return []types.AnthropicContentBlock{{
			Type: "text",
			Text: strings.TrimSpace(string(raw)),
		}}, nil
	}
	blocks := make([]types.AnthropicContentBlock, 0, len(results))
	for _, result := range results {
		block := types.AnthropicContentBlock{
			Type:        "search_result",
			ExtraFields: map[string]json.RawMessage{},
		}
		if name, _ := result["file_name"].(string); strings.TrimSpace(name) != "" {
			block.ExtraFields["title"] = mustMarshalRaw(name)
		}
		if fileID, _ := result["file_id"].(string); strings.TrimSpace(fileID) != "" {
			block.ExtraFields["file_id"] = mustMarshalRaw(fileID)
		}
		if score, ok := result["score"]; ok {
			block.ExtraFields["score"] = mustMarshalRaw(score)
		}
		if content, ok := result["content"].([]any); ok {
			var textParts []string
			for _, entry := range content {
				asMap, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				if text, _ := asMap["text"].(string); strings.TrimSpace(text) != "" {
					textParts = append(textParts, text)
				}
			}
			if len(textParts) > 0 {
				block.Text = strings.Join(textParts, "\n")
			}
		}
		if rawResult, err := json.Marshal(result); err == nil {
			block.ExtraFields["search_result"] = rawResult
		}
		if len(block.ExtraFields) == 0 {
			block.ExtraFields = nil
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func openAIMcpCallToBlocks(item types.OpenAIOutputItem, mode config.Mode) ([]types.AnthropicContentBlock, error) {
	callBlock, err := openAISpecialToolCallToBlock(item, mode, "MCP")
	if err != nil {
		return nil, err
	}
	blocks := []types.AnthropicContentBlock{callBlock}
	resultBlock, err := openAIMcpCallResultToBlock(item, mode)
	if err != nil {
		return nil, err
	}
	if resultBlock != nil {
		blocks = append(blocks, *resultBlock)
	}
	return blocks, nil
}

func openAIMcpCallResultToBlock(item types.OpenAIOutputItem, mode config.Mode) (*types.AnthropicContentBlock, error) {
	if len(item.Output) == 0 && len(item.Error) == 0 {
		return nil, nil
	}
	content, isErr, extra, err := mcpPayloadToToolResult(item.Output, item.Error, mode)
	if err != nil {
		return nil, err
	}
	block := &types.AnthropicContentBlock{
		Type:      "tool_result",
		ToolUseID: firstNonEmpty(item.CallID, item.ID),
		Content:   content,
		Status:    item.Status,
	}
	if isErr {
		block.IsError = boolPtr(true)
	}
	if len(extra) > 0 {
		block.ExtraFields = extra
	}
	attachStoredResponseItem(block, item)
	return block, nil
}

func openAIComputerCallOutputToBlock(item types.OpenAIOutputItem, mode config.Mode) (*types.AnthropicContentBlock, error) {
	toolUseID := strings.TrimSpace(item.CallID)
	if toolUseID == "" {
		if mode == config.ModeStrict {
			return nil, errors.New("computer_call_output missing call_id")
		}
		return nil, nil
	}

	content := json.RawMessage(`""`)
	if len(item.Output) > 0 {
		if rendered, ok, err := computerOutputToAnthropicContent(item.Output, mode); err != nil {
			return nil, err
		} else if ok {
			content = rendered
		} else if mode == config.ModeStrict {
			return nil, errors.New("unsupported computer_call_output payload")
		} else {
			content = mustMarshalRaw(strings.TrimSpace(string(item.Output)))
		}
	}

	block := &types.AnthropicContentBlock{
		Type:      "tool_result",
		ToolUseID: toolUseID,
		Content:   content,
		Status:    item.Status,
	}
	attachStoredResponseItem(block, item)
	if len(item.AcknowledgedSafetyChecks) > 0 {
		if block.ExtraFields == nil {
			block.ExtraFields = map[string]json.RawMessage{}
		}
		block.ExtraFields["acknowledged_safety_checks"] = cloneRaw(item.AcknowledgedSafetyChecks)
	}
	return block, nil
}

func computerOutputToAnthropicContent(raw json.RawMessage, mode config.Mode) (json.RawMessage, bool, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		if mode == config.ModeStrict {
			return nil, false, err
		}
		return mustMarshalRaw(strings.TrimSpace(string(raw))), true, nil
	}
	switch strings.TrimSpace(fmt.Sprint(payload["type"])) {
	case "computer_screenshot":
		source := types.AnthropicImage{}
		if fileID, _ := payload["file_id"].(string); strings.TrimSpace(fileID) != "" {
			source.Type = "file_id"
			source.FileID = fileID
		} else if imageURL, _ := payload["image_url"].(string); strings.TrimSpace(imageURL) != "" {
			source.Type = "url"
			source.URL = imageURL
		} else {
			if mode == config.ModeStrict {
				return nil, false, errors.New("computer_screenshot missing file_id or image_url")
			}
			return mustMarshalRaw(strings.TrimSpace(string(raw))), true, nil
		}
		return mustMarshalRaw([]types.AnthropicContentBlock{{
			Type:   "image",
			Source: &source,
		}}), true, nil
	default:
		return mustMarshalRaw(strings.TrimSpace(string(raw))), true, nil
	}
}

func mcpPayloadToToolResult(outputRaw, errorRaw json.RawMessage, mode config.Mode) (json.RawMessage, bool, map[string]json.RawMessage, error) {
	isErr := len(errorRaw) > 0
	extra := map[string]json.RawMessage{}
	if len(errorRaw) > 0 {
		extra["error"] = cloneRaw(errorRaw)
	}
	if len(outputRaw) == 0 {
		if len(errorRaw) == 0 {
			return json.RawMessage(`""`), false, nil, nil
		}
		return mustMarshalRaw(strings.TrimSpace(string(errorRaw))), true, extra, nil
	}

	var outputObj map[string]json.RawMessage
	if err := json.Unmarshal(outputRaw, &outputObj); err != nil {
		if mode == config.ModeStrict {
			return nil, false, nil, err
		}
		return mustMarshalRaw(strings.TrimSpace(string(outputRaw))), isErr, extra, nil
	}

	if raw, ok := outputObj["structuredContent"]; ok {
		extra["structuredContent"] = cloneRaw(raw)
	}
	if raw, ok := outputObj["_meta"]; ok {
		extra["_meta"] = cloneRaw(raw)
	}
	if raw, ok := outputObj["isError"]; ok {
		var parsed bool
		if json.Unmarshal(raw, &parsed) == nil && parsed {
			isErr = true
		}
		extra["isError"] = cloneRaw(raw)
	}
	if raw, ok := outputObj["content"]; ok {
		blocks, err := genericToolContentToAnthropicBlocks(raw, mode)
		if err != nil {
			return nil, false, nil, err
		}
		if len(blocks) > 0 {
			if len(extra) == 0 {
				extra = nil
			}
			return mustMarshalRaw(blocks), isErr, extra, nil
		}
	}
	if len(extra) == 0 {
		extra = nil
	}
	return mustMarshalRaw(strings.TrimSpace(string(outputRaw))), isErr, extra, nil
}

func genericToolContentToAnthropicBlocks(raw json.RawMessage, mode config.Mode) ([]types.AnthropicContentBlock, error) {
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		if mode == config.ModeStrict {
			return nil, err
		}
		return []types.AnthropicContentBlock{{
			Type: "text",
			Text: strings.TrimSpace(string(raw)),
		}}, nil
	}

	blocks := make([]types.AnthropicContentBlock, 0, len(items))
	for _, item := range items {
		itemType, _ := item["type"].(string)
		switch itemType {
		case "text", "input_text", "output_text":
			text, _ := item["text"].(string)
			if strings.TrimSpace(text) == "" {
				if rawText, err := json.Marshal(item); err == nil {
					text = string(rawText)
				}
			}
			blocks = append(blocks, types.AnthropicContentBlock{Type: "text", Text: text})
		case "image", "input_image":
			source := &types.AnthropicImage{}
			if fileID, _ := item["file_id"].(string); strings.TrimSpace(fileID) != "" {
				source.Type = "file_id"
				source.FileID = fileID
			} else if imageURL, _ := item["image_url"].(string); strings.TrimSpace(imageURL) != "" {
				source.Type = "url"
				source.URL = imageURL
			} else if url, _ := item["url"].(string); strings.TrimSpace(url) != "" {
				source.Type = "url"
				source.URL = url
			} else {
				if mode == config.ModeStrict {
					return nil, fmt.Errorf("unsupported tool content image payload: %#v", item)
				}
				rawItem, _ := json.Marshal(item)
				blocks = append(blocks, types.AnthropicContentBlock{Type: "text", Text: string(rawItem)})
				continue
			}
			blocks = append(blocks, types.AnthropicContentBlock{Type: "image", Source: source})
		case "input_file", "file", "document":
			source := &types.AnthropicImage{}
			if fileID, _ := item["file_id"].(string); strings.TrimSpace(fileID) != "" {
				source.Type = "file_id"
				source.FileID = fileID
			} else if fileURL, _ := item["file_url"].(string); strings.TrimSpace(fileURL) != "" {
				source.Type = "url"
				source.URL = fileURL
			} else {
				rawItem, _ := json.Marshal(item)
				blocks = append(blocks, types.AnthropicContentBlock{Type: "text", Text: string(rawItem)})
				continue
			}
			block := types.AnthropicContentBlock{Type: "document", Source: source}
			if filename, _ := item["filename"].(string); strings.TrimSpace(filename) != "" {
				block.Name = filename
			}
			blocks = append(blocks, block)
		default:
			rawItem, _ := json.Marshal(item)
			blocks = append(blocks, types.AnthropicContentBlock{Type: "text", Text: string(rawItem)})
		}
	}
	return blocks, nil
}

func boolPtr(v bool) *bool { return &v }

func parseToolLikeInputJSON(raw string, out *map[string]any) error {
	if strings.TrimSpace(raw) == "" {
		*out = map[string]any{}
		return nil
	}
	return json.Unmarshal([]byte(raw), out)
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
	attachStoredResponseItem(&serverToolUse, item)

	// web_search_tool_result 块（从注解构建搜索结果）
	searchResults := annotationsToSearchResults(annotations)
	resultBlock := types.AnthropicContentBlock{
		Type:      "web_search_tool_result",
		ToolUseID: toolUseID,
		Content:   mustMarshalRaw(searchResults),
	}

	return []types.AnthropicContentBlock{serverToolUse, resultBlock}
}

func attachStoredResponseItem(block *types.AnthropicContentBlock, item types.OpenAIOutputItem) {
	raw, err := json.Marshal(item)
	if err != nil {
		return
	}
	if block.ExtraFields == nil {
		block.ExtraFields = make(map[string]json.RawMessage)
	}
	block.ExtraFields[storedResponsesItemExtraKey] = raw
}

func firstRawString(extra map[string]json.RawMessage, key string) string {
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

func openAICompactionToBlock(item types.OpenAIOutputItem, mode config.Mode) (*types.AnthropicContentBlock, error) {
	if strings.TrimSpace(item.EncryptedContent) == "" {
		if mode == config.ModeStrict {
			return nil, errors.New("compaction item missing encrypted_content")
		}
		return nil, nil
	}
	return &types.AnthropicContentBlock{
		Type:    "compaction",
		Content: mustMarshalRaw(item.EncryptedContent),
	}, nil
}

func openAIImageGenerationToBlock(item types.OpenAIOutputItem) types.AnthropicContentBlock {
	return types.AnthropicContentBlock{
		Type:          "image_generation_call",
		ID:            item.ID,
		Status:        item.Status,
		RevisedPrompt: item.RevisedPrompt,
		Result:        item.Result,
	}
}

func openAIOpaqueOutputToBlock(item types.OpenAIOutputItem) (*types.AnthropicContentBlock, error) {
	if strings.TrimSpace(item.Type) == "" {
		return nil, errors.New("opaque output item missing type")
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return nil, err
	}
	return &types.AnthropicContentBlock{
		Type:          "responses_output_item",
		ResponsesType: item.Type,
		Content:       raw,
	}, nil
}

func toolSearchToolsToReferences(raw json.RawMessage, mode config.Mode) ([]map[string]any, error) {
	if len(raw) == 0 {
		return []map[string]any{}, nil
	}

	var items []any
	if err := json.Unmarshal(raw, &items); err != nil {
		if mode == config.ModeStrict {
			return nil, fmt.Errorf("invalid tool_search_output tools: %w", err)
		}
		return []map[string]any{}, nil
	}

	var refs []map[string]any
	seen := make(map[string]struct{})
	var walk func(v any)
	walk = func(v any) {
		switch val := v.(type) {
		case map[string]any:
			if typ, _ := val["type"].(string); typ == "function" {
				if name, _ := val["name"].(string); strings.TrimSpace(name) != "" {
					if _, ok := seen[name]; !ok {
						seen[name] = struct{}{}
						refs = append(refs, map[string]any{
							"type":      "tool_reference",
							"tool_name": name,
						})
					}
				}
			}
			if nested, ok := val["tools"].([]any); ok {
				for _, child := range nested {
					walk(child)
				}
			}
		case []any:
			for _, child := range val {
				walk(child)
			}
		}
	}
	walk(items)
	return refs, nil
}

func openAIArgumentsString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	return strings.TrimSpace(string(raw))
}

func unwrapShellCommand(parts []string) string {
	if len(parts) >= 3 {
		head := strings.ToLower(strings.TrimSpace(parts[0]))
		flag := strings.TrimSpace(parts[1])
		switch {
		case (head == "bash" || head == "sh" || head == "/bin/bash" || head == "/bin/sh") && flag == "-lc":
			return parts[2]
		case strings.Contains(head, "powershell") && strings.EqualFold(flag, "-Command"):
			return parts[2]
		}
	}
	return strings.Join(parts, " ")
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
