package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"cdx.cc/claude-bridge/internal/config"
	"cdx.cc/claude-bridge/internal/tokenizer"
	"cdx.cc/claude-bridge/internal/transform"
	"cdx.cc/claude-bridge/internal/types"
)

func (s *Server) prepareAutoCompact(
	ctx context.Context,
	r *http.Request,
	oaReq types.OpenAIResponsesRequest,
	estimatedInputTokens int,
) (types.OpenAIResponsesRequest, int) {
	autoCfg := s.rtCfg.GetAutoCompact()
	if autoCfg.Mode == config.AutoCompactOff || autoCfg.ThresholdTokens <= 0 {
		return oaReq, estimatedInputTokens
	}
	if estimatedInputTokens < autoCfg.ThresholdTokens {
		return oaReq, estimatedInputTokens
	}

	switch autoCfg.Mode {
	case config.AutoCompactContextManagement:
		req, applied := applyContextManagementAutoCompact(oaReq, autoCfg.ThresholdTokens)
		if applied {
			s.log.Debug("auto compact threshold reached; injecting context_management",
				slog.Int("estimated_input_tokens", estimatedInputTokens),
				slog.Int("threshold_tokens", autoCfg.ThresholdTokens),
			)
		}
		return req, estimatedInputTokens
	case config.AutoCompactResponsesCompact:
		req, newEstimate, ok := s.applyRemoteAutoCompact(ctx, r, oaReq, autoCfg, estimatedInputTokens)
		if ok {
			return req, newEstimate
		}
		return oaReq, estimatedInputTokens
	default:
		return oaReq, estimatedInputTokens
	}
}

func applyContextManagementAutoCompact(oaReq types.OpenAIResponsesRequest, threshold int) (types.OpenAIResponsesRequest, bool) {
	if threshold <= 0 {
		return oaReq, false
	}
	if len(oaReq.ContextManagement) == 0 {
		oaReq.ContextManagement = mustMarshalRaw([]map[string]any{{
			"type":              "compaction",
			"compact_threshold": threshold,
		}})
		return oaReq, true
	}

	var items []map[string]any
	if err := json.Unmarshal(oaReq.ContextManagement, &items); err != nil {
		return oaReq, false
	}
	for _, item := range items {
		if itemType, _ := item["type"].(string); itemType == "compaction" {
			if _, ok := item["compact_threshold"]; !ok {
				item["compact_threshold"] = threshold
				oaReq.ContextManagement = mustMarshalRaw(items)
				return oaReq, true
			}
			return oaReq, false
		}
	}
	items = append(items, map[string]any{
		"type":              "compaction",
		"compact_threshold": threshold,
	})
	oaReq.ContextManagement = mustMarshalRaw(items)
	return oaReq, true
}

func (s *Server) applyRemoteAutoCompact(
	ctx context.Context,
	r *http.Request,
	oaReq types.OpenAIResponsesRequest,
	autoCfg config.AutoCompactConfig,
	estimatedInputTokens int,
) (types.OpenAIResponsesRequest, int, bool) {
	payload := map[string]any{
		"model":               oaReq.Model,
		"input":               oaReq.Input,
		"instructions":        oaReq.Instructions,
		"tools":               oaReq.Tools,
		"parallel_tool_calls": boolValue(oaReq.ParallelToolCalls),
	}
	if oaReq.Reasoning != nil {
		payload["reasoning"] = oaReq.Reasoning
	}
	if len(oaReq.Text) > 0 {
		var text any
		if err := json.Unmarshal(oaReq.Text, &text); err == nil {
			payload["text"] = text
		}
	}

	headers := s.forwardHeaders(r)
	resp, data, err := s.upstream.DoJSON(ctx, "/v1/responses/compact", payload, headers)
	if err != nil {
		s.log.Warn("remote auto compact failed; continuing without compaction",
			slog.Any("err", err),
			slog.Int("estimated_input_tokens", estimatedInputTokens),
			slog.Int("threshold_tokens", autoCfg.ThresholdTokens),
		)
		return oaReq, estimatedInputTokens, false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.log.Warn("remote auto compact returned non-2xx; continuing without compaction",
			slog.Int("status", resp.StatusCode),
			slog.String("body", truncateLogBody(data)),
		)
		return oaReq, estimatedInputTokens, false
	}

	var compactResp struct {
		Output []types.OpenAIOutputItem `json:"output"`
	}
	if err := json.Unmarshal(data, &compactResp); err != nil {
		s.log.Warn("remote auto compact decode failed; continuing without compaction", slog.Any("err", err))
		return oaReq, estimatedInputTokens, false
	}
	compactedInput, err := transform.OutputItemsToInputItems(compactResp.Output)
	if err != nil {
		s.log.Warn("remote auto compact output conversion failed; continuing without compaction", slog.Any("err", err))
		return oaReq, estimatedInputTokens, false
	}

	oaReq.Input = compactedInput
	oaReq.PreviousResponseID = ""
	oaReq.ContextManagement = nil
	newEstimate := tokenizer.CountOpenAIResponsesRequest(oaReq)
	s.log.Debug("remote auto compact applied",
		slog.Int("before_tokens", estimatedInputTokens),
		slog.Int("after_tokens", newEstimate),
		slog.Int("threshold_tokens", autoCfg.ThresholdTokens),
	)
	return oaReq, newEstimate, true
}

func boolValue(v *bool) bool {
	return v != nil && *v
}

func truncateLogBody(data []byte) string {
	if len(data) <= 300 {
		return string(data)
	}
	return string(data[:300]) + "..."
}

func mustMarshalRaw(val any) json.RawMessage {
	data, _ := json.Marshal(val)
	return data
}
