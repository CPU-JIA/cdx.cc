package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cdx.cc/claude-bridge/internal/config"
	"cdx.cc/claude-bridge/internal/types"
)

func TestResponsesCompactPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses/compact" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		if got := r.Header.Get("OpenAI-Beta"); got != "responses_websockets=2026-02-06" {
			t.Fatalf("expected OpenAI-Beta header to be forwarded, got %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		if payload["model"] != "gpt-5.4" {
			t.Fatalf("expected model to be remapped, got %#v", payload["model"])
		}
		if payload["prompt_cache_retention"] != "24h" {
			t.Fatalf("expected prompt_cache_retention=24h, got %#v", payload["prompt_cache_retention"])
		}
		if key, _ := payload["prompt_cache_key"].(string); strings.TrimSpace(key) == "" {
			t.Fatalf("expected derived prompt_cache_key, got %#v", payload["prompt_cache_key"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req_upstream_123")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":[{"type":"compaction","encrypted_content":"enc_123"}]}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, map[string]config.ModelMapping{
		"claude-sonnet-4-6": {UpstreamModel: "gpt-5.4"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{
		"model":"claude-sonnet-4-6",
		"input":[],
		"instructions":"compact this",
		"tools":[],
		"parallel_tool_calls":false
	}`))
	req.Header.Set("Authorization", "Bearer "+srv.rtCfg.GetAuthToken())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses_websockets=2026-02-06")

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("request-id"); got != "req_upstream_123" {
		t.Fatalf("expected upstream request-id, got %q", got)
	}
	if strings.TrimSpace(rec.Body.String()) != `{"output":[{"type":"compaction","encrypted_content":"enc_123"}]}` {
		t.Fatalf("unexpected response body: %s", rec.Body.String())
	}
}

func TestSetAnthropicHeadersIncludesUnifiedQuotaHeaders(t *testing.T) {
	srv := newTestServer(t, "https://api.openai.com", nil)
	rec := httptest.NewRecorder()

	srv.setAnthropicHeaders(rec, 100, 50, 0, 0, "req_test_123")

	if got := rec.Header().Get("anthropic-ratelimit-unified-status"); got != "allowed" {
		t.Fatalf("expected unified status header, got %q", got)
	}
	if got := rec.Header().Get("anthropic-ratelimit-unified-5h-utilization"); got != "7" {
		t.Fatalf("expected 5h utilization header, got %q", got)
	}
	if got := rec.Header().Get("anthropic-ratelimit-unified-7d-utilization"); got != "7" {
		t.Fatalf("expected 7d utilization header, got %q", got)
	}
	if got := rec.Header().Get("request-id"); got != "req_test_123" {
		t.Fatalf("expected request-id header, got %q", got)
	}
}

func TestHandleMessagesMapsUpstreamErrorObject(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req_upstream_error_1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exceeded","type":"rate_limit_error","code":"rate_limit_exceeded"}}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, map[string]config.ModelMapping{
		"claude-sonnet-4-6": {UpstreamModel: "gpt-5.4"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":128,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Authorization", "Bearer "+srv.rtCfg.GetAuthToken())
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("request-id"); got != "req_upstream_error_1" {
		t.Fatalf("expected upstream request-id, got %q", got)
	}
	var payload types.AnthropicError
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if payload.Error.Type != "rate_limit_error" || payload.Error.Message != "quota exceeded" {
		t.Fatalf("unexpected mapped error payload: %#v", payload)
	}
}

func TestPrepareAutoCompactInjectsContextManagement(t *testing.T) {
	srv := newTestServer(t, "https://api.openai.com", nil)
	current := srv.rtCfg.Get()
	current.AutoCompact = config.AutoCompactConfig{
		Mode:            config.AutoCompactContextManagement,
		ThresholdTokens: 100,
	}
	if err := srv.rtCfg.Update(current); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	oaReq := types.OpenAIResponsesRequest{
		Model: "gpt-5.4",
		Input: []types.OpenAIInputItem{{Type: "message", Role: "user"}},
	}

	got, estimated := srv.prepareAutoCompact(context.Background(), req, oaReq, 120)
	if estimated != 120 {
		t.Fatalf("expected estimate unchanged, got %d", estimated)
	}
	if len(got.ContextManagement) == 0 {
		t.Fatalf("expected context_management to be injected")
	}
	if !strings.Contains(string(got.ContextManagement), `"compact_threshold":100`) {
		t.Fatalf("unexpected context_management: %s", string(got.ContextManagement))
	}
}

func TestPrepareAutoCompactCallsRemoteCompact(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses/compact" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"output":[
				{
					"type":"message",
					"role":"assistant",
					"content":[{"type":"output_text","text":"Compacted summary"}]
				}
			]
		}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, nil)
	current := srv.rtCfg.Get()
	current.AutoCompact = config.AutoCompactConfig{
		Mode:            config.AutoCompactResponsesCompact,
		ThresholdTokens: 100,
	}
	if err := srv.rtCfg.Update(current); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	oaReq := types.OpenAIResponsesRequest{
		Model: "gpt-5.4",
		Input: []types.OpenAIInputItem{
			{Type: "message", Role: "user", Content: []types.OpenAIInputContent{{Type: "input_text", Text: "hello"}}},
			{Type: "message", Role: "assistant", Content: []types.OpenAIInputContent{{Type: "output_text", Text: "world"}}},
		},
	}

	got, estimated := srv.prepareAutoCompact(context.Background(), req, oaReq, 120)
	if len(got.Input) != 1 {
		t.Fatalf("expected compacted input length 1, got %#v", got.Input)
	}
	if got.Input[0].Role != "assistant" || len(got.Input[0].Content) != 1 || got.Input[0].Content[0].Text != "Compacted summary" {
		t.Fatalf("unexpected compacted input: %#v", got.Input[0])
	}
	if estimated <= 0 {
		t.Fatalf("expected positive post-compact estimate, got %d", estimated)
	}
}

func TestHandleMessagesRetriesWithoutPromptCacheRetentionWhenUnsupported(t *testing.T) {
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if attempts == 1 {
			if payload["prompt_cache_retention"] != "24h" {
				t.Fatalf("expected first attempt to include 24h retention, got %#v", payload["prompt_cache_retention"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"prompt_cache_retention not supported","type":"invalid_request_error"}}`))
			return
		}
		if _, ok := payload["prompt_cache_retention"]; ok {
			t.Fatalf("expected retry without prompt_cache_retention, got %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_1","model":"gpt-5.4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":2},"status":"completed"}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, map[string]config.ModelMapping{
		"claude-sonnet-4-6": {UpstreamModel: "gpt-5.4"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":128,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Authorization", "Bearer "+srv.rtCfg.GetAuthToken())
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
}

func TestHandleMessagesRetriesWithoutPromptCacheFieldsOnOpaqueUpstreamError(t *testing.T) {
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if attempts == 1 {
			if _, ok := payload["prompt_cache_key"]; !ok {
				t.Fatalf("expected first attempt to include prompt_cache_key, got %#v", payload)
			}
			if payload["prompt_cache_retention"] != "24h" {
				t.Fatalf("expected first attempt to include 24h retention, got %#v", payload["prompt_cache_retention"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"Upstream request failed","type":"upstream_error"}}`))
			return
		}
		if _, ok := payload["prompt_cache_key"]; ok {
			t.Fatalf("expected retry without prompt_cache_key, got %#v", payload)
		}
		if _, ok := payload["prompt_cache_retention"]; ok {
			t.Fatalf("expected retry without prompt_cache_retention, got %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_opaque_retry","model":"gpt-5.4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":2},"status":"completed"}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, map[string]config.ModelMapping{
		"claude-sonnet-4-6": {UpstreamModel: "gpt-5.4"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":128,
		"metadata":{"user_id":"{\"device_id\":\"dev_retry\",\"session_id\":\"sess_retry\"}"},
		"messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Authorization", "Bearer "+srv.rtCfg.GetAuthToken())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-code/test")

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
}

func TestHandleMessagesStreamRetriesWithoutPromptCacheFieldsOnOpaqueUpstreamError(t *testing.T) {
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if attempts == 1 {
			if _, ok := payload["prompt_cache_key"]; !ok {
				t.Fatalf("expected first attempt to include prompt_cache_key, got %#v", payload)
			}
			if payload["prompt_cache_retention"] != "24h" {
				t.Fatalf("expected first attempt to include 24h retention, got %#v", payload["prompt_cache_retention"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"Upstream request failed","type":"upstream_error"}}`))
			return
		}
		if _, ok := payload["prompt_cache_key"]; ok {
			t.Fatalf("expected retry without prompt_cache_key, got %#v", payload)
		}
		if _, ok := payload["prompt_cache_retention"]; ok {
			t.Fatalf("expected retry without prompt_cache_retention, got %#v", payload)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", `{"type":"response.completed","response":{"id":"resp_stream_retry","model":"gpt-5.4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":2},"status":"completed"}}`)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, map[string]config.ModelMapping{
		"claude-sonnet-4-6": {UpstreamModel: "gpt-5.4"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":128,
		"stream":true,
		"metadata":{"user_id":"{\"device_id\":\"dev_stream_retry\",\"session_id\":\"sess_stream_retry\"}"},
		"messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Authorization", "Bearer "+srv.rtCfg.GetAuthToken())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-code/test")

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if !strings.Contains(rec.Body.String(), `"message_stop"`) {
		t.Fatalf("expected bridged stream output, got %s", rec.Body.String())
	}
}

func TestHandleMessagesRetriesWithoutReasoningOnAccountExhaustion(t *testing.T) {
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if attempts == 1 {
			if _, ok := payload["reasoning"]; !ok {
				t.Fatalf("expected first attempt to include reasoning, got %#v", payload)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"All available accounts exhausted","code":"server_error"}}`))
			return
		}
		if _, ok := payload["reasoning"]; ok {
			t.Fatalf("expected retry without reasoning, got %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_reasoning_retry","model":"gpt-5.4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":2},"status":"completed"}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, map[string]config.ModelMapping{
		"claude-sonnet-4-6": {UpstreamModel: "gpt-5.4", ReasoningEffort: "high"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":128,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Authorization", "Bearer "+srv.rtCfg.GetAuthToken())
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
}

func TestHandleMessagesStreamRetriesWithoutReasoningOnAccountExhaustion(t *testing.T) {
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if attempts == 1 {
			if _, ok := payload["reasoning"]; !ok {
				t.Fatalf("expected first attempt to include reasoning, got %#v", payload)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"All available accounts exhausted","code":"server_error"}}`))
			return
		}
		if _, ok := payload["reasoning"]; ok {
			t.Fatalf("expected retry without reasoning, got %#v", payload)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", `{"type":"response.completed","response":{"id":"resp_stream_reasoning_retry","model":"gpt-5.4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":2},"status":"completed"}}`)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, map[string]config.ModelMapping{
		"claude-sonnet-4-6": {UpstreamModel: "gpt-5.4", ReasoningEffort: "high"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":128,
		"stream":true,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Authorization", "Bearer "+srv.rtCfg.GetAuthToken())
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if !strings.Contains(rec.Body.String(), `"message_stop"`) {
		t.Fatalf("expected bridged stream output, got %s", rec.Body.String())
	}
}

func TestHandleMessagesDerivesPromptCacheKeyFromMetadata(t *testing.T) {
	var seenKeys []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		key, _ := payload["prompt_cache_key"].(string)
		seenKeys = append(seenKeys, key)
		if payload["prompt_cache_retention"] != "24h" {
			t.Fatalf("expected prompt_cache_retention=24h, got %#v", payload["prompt_cache_retention"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_cache_1","model":"gpt-5.4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":12,"output_tokens":2},"status":"completed"}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, map[string]config.ModelMapping{
		"claude-sonnet-4-6": {UpstreamModel: "gpt-5.4"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":128,
		"metadata":{"user_id":"{\"device_id\":\"dev_123\",\"session_id\":\"sess_456\"}"},
		"messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Authorization", "Bearer "+srv.rtCfg.GetAuthToken())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-code/test")

	rec1 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec1, req)
	if rec1.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec1.Code, rec1.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":128,
		"metadata":{"user_id":"{\"device_id\":\"dev_123\",\"session_id\":\"sess_999\"}"},
		"messages":[{"role":"user","content":"hello again"}]
	}`))
	req2.Header.Set("Authorization", "Bearer "+srv.rtCfg.GetAuthToken())
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("User-Agent", "claude-code/test")
	rec2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec2.Code, rec2.Body.String())
	}

	if len(seenKeys) != 2 || strings.TrimSpace(seenKeys[0]) == "" {
		t.Fatalf("expected prompt_cache_key to be generated")
	}
	if seenKeys[0] != seenKeys[1] {
		t.Fatalf("expected stable device-based prompt_cache_key, got %#v", seenKeys)
	}
}

func TestHandleMessagesRespectsPromptCacheConfigOff(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if _, ok := payload["prompt_cache_key"]; ok {
			t.Fatalf("expected no prompt_cache_key when prompt cache is off, got %#v", payload)
		}
		if _, ok := payload["prompt_cache_retention"]; ok {
			t.Fatalf("expected no prompt_cache_retention when prompt cache is off, got %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_off","model":"gpt-5.4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":1},"status":"completed"}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, map[string]config.ModelMapping{
		"claude-sonnet-4-6": {UpstreamModel: "gpt-5.4"},
	})
	current := srv.rtCfg.Get()
	current.PromptCache = config.PromptCacheConfig{Mode: config.PromptCacheOff, AutoKey: false}
	if err := srv.rtCfg.Update(current); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":64,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Authorization", "Bearer "+srv.rtCfg.GetAuthToken())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleMessagesAutoPromptCacheNeedsSignalWhenAutoKeyDisabled(t *testing.T) {
	var seenRetention any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		seenRetention = payload["prompt_cache_retention"]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_auto","model":"gpt-5.4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":1},"status":"completed"}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, map[string]config.ModelMapping{
		"claude-sonnet-4-6": {UpstreamModel: "gpt-5.4"},
	})
	current := srv.rtCfg.Get()
	current.PromptCache = config.PromptCacheConfig{Mode: config.PromptCacheAuto, AutoKey: false}
	if err := srv.rtCfg.Update(current); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":64,
		"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]
	}`))
	req.Header.Set("Authorization", "Bearer "+srv.rtCfg.GetAuthToken())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if seenRetention != "24h" {
		t.Fatalf("expected auto mode to inject 24h on Anthropic cache marker, got %#v", seenRetention)
	}
}

func TestHandleMessagesBackfillsAnthropicCacheCreationBuckets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id":"resp_cache_usage",
			"model":"gpt-5.4",
			"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],
			"usage":{"input_tokens":20,"output_tokens":5,"input_tokens_details":{"cached_tokens":12}},
			"status":"completed"
		}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, map[string]config.ModelMapping{
		"claude-sonnet-4-6": {UpstreamModel: "gpt-5.4"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":64,
		"prompt_cache_retention":"in_memory",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Authorization", "Bearer "+srv.rtCfg.GetAuthToken())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp types.AnthropicMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.Usage.CacheReadInputTokens != 12 {
		t.Fatalf("expected cache_read_input_tokens=12, got %#v", resp.Usage)
	}
	if resp.Usage.CacheCreation == nil || resp.Usage.CacheCreation.Ephemeral5MInputTokens != 12 {
		t.Fatalf("expected 5m cache bucket backfill, got %#v", resp.Usage.CacheCreation)
	}
}

func newTestServer(t *testing.T, upstreamURL string, modelMap map[string]config.ModelMapping) *Server {
	t.Helper()
	t.Setenv("AUTH_TOKEN", "sk-cdx.cc-test-auth")

	cfg := config.Config{
		ListenAddr:      ":0",
		UpstreamBaseURL: upstreamURL,
		Mode:            config.ModeBestEffort,
		Timeout:         5 * time.Second,
		MaxBodyBytes:    1024 * 1024,
		LogLevel:        "error",
		ModelMap:        modelMap,
		ContextLimit:    1024,
		OutputLimit:     1024,
		PromptCache:     config.DefaultPromptCacheConfig(),
	}
	rc, err := config.NewRuntimeConfig(cfg, filepath.Join(t.TempDir(), "runtime_config.json"))
	if err != nil {
		t.Fatalf("NewRuntimeConfig() error = %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, rc, logger)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Close()
	})
	return srv
}
