package transform

import (
	"encoding/json"
	"strings"

	"cdx.cc/claude-bridge/internal/types"
)

func OutputItemsToInputItems(items []types.OpenAIOutputItem) ([]types.OpenAIInputItem, error) {
	out := make([]types.OpenAIInputItem, 0, len(items))
	for _, item := range items {
		input, err := outputItemToInputItem(item)
		if err != nil {
			return nil, err
		}
		out = append(out, input)
	}
	return out, nil
}

func outputItemToInputItem(item types.OpenAIOutputItem) (types.OpenAIInputItem, error) {
	extra := cloneRawMap(item.ExtraFields)
	input := types.OpenAIInputItem{
		Type:             item.Type,
		ID:               item.ID,
		Role:             item.Role,
		CallID:           item.CallID,
		Name:             item.Name,
		Namespace:        item.Namespace,
		Arguments:        cloneRaw(item.Arguments),
		Input:            cloneRaw(item.Input),
		Error:            cloneRaw(item.Error),
		Action:           cloneRaw(item.Action),
		Queries:          append([]string(nil), item.Queries...),
		Results:          cloneRaw(item.Results),
		Status:           item.Status,
		Execution:        item.Execution,
		EncryptedContent: item.EncryptedContent,
		RevisedPrompt:    item.RevisedPrompt,
		Result:           item.Result,
		Phase:            item.Phase,
		EndTurn:          item.EndTurn,
		ExtraFields:      extra,
	}
	if len(item.Tools) > 0 {
		var tools []any
		if err := json.Unmarshal(item.Tools, &tools); err == nil {
			input.Tools = tools
		} else {
			if input.ExtraFields == nil {
				input.ExtraFields = map[string]json.RawMessage{}
			}
			input.ExtraFields["tools"] = cloneRaw(item.Tools)
		}
	}

	switch item.Type {
	case "message":
		delete(input.ExtraFields, "content")
		input.Content = messageOutputContentToInput(item.Content)
	case "reasoning":
		if input.ExtraFields == nil {
			input.ExtraFields = map[string]json.RawMessage{}
		}
		if len(item.Content) > 0 {
			input.ExtraFields["content"] = cloneRaw(item.Content)
		}
		if len(item.Summary) > 0 {
			input.ExtraFields["summary"] = cloneRaw(item.Summary)
		}
	case "computer_call_output":
		if input.ExtraFields == nil {
			input.ExtraFields = map[string]json.RawMessage{}
		}
		if len(item.Output) > 0 {
			var output any
			if err := json.Unmarshal(item.Output, &output); err == nil {
				input.Output = output
			} else {
				input.ExtraFields["output"] = cloneRaw(item.Output)
			}
		}
		if len(item.AcknowledgedSafetyChecks) > 0 {
			input.ExtraFields["acknowledged_safety_checks"] = cloneRaw(item.AcknowledgedSafetyChecks)
		}
	case "function_call_output", "custom_tool_call_output":
		rawOutput := cloneRaw(item.Output)
		if len(rawOutput) == 0 {
			rawOutput = input.ExtraFields["output"]
		}
		if len(rawOutput) > 0 {
			var output any
			if err := json.Unmarshal(rawOutput, &output); err == nil {
				input.Output = output
				delete(input.ExtraFields, "output")
			}
		}
	default:
		if len(item.Output) > 0 {
			input.Output = cloneRaw(item.Output)
		}
	}
	if len(input.ExtraFields) == 0 {
		input.ExtraFields = nil
	}
	return input, nil
}

func messageOutputContentToInput(raw json.RawMessage) []types.OpenAIInputContent {
	if len(raw) == 0 {
		return nil
	}
	var parts []types.OpenAIMessageContent
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]types.OpenAIInputContent, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "output_text":
			out = append(out, types.OpenAIInputContent{
				Type: "output_text",
				Text: part.Text,
			})
		case "text":
			out = append(out, types.OpenAIInputContent{
				Type: "output_text",
				Text: part.Text,
			})
		case "refusal":
			text := strings.TrimSpace(part.Refusal)
			if text == "" {
				text = "[refusal]"
			}
			out = append(out, types.OpenAIInputContent{
				Type:    "refusal",
				Refusal: text,
			})
		}
	}
	return out
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func cloneRawMap(src map[string]json.RawMessage) map[string]json.RawMessage {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]json.RawMessage, len(src))
	for key, val := range src {
		dst[key] = cloneRaw(val)
	}
	return dst
}
