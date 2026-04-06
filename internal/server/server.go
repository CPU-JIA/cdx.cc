package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"cdx.cc/claude-bridge/internal/admin"
	"cdx.cc/claude-bridge/internal/config"
	"cdx.cc/claude-bridge/internal/sse"
	"cdx.cc/claude-bridge/internal/store"
	"cdx.cc/claude-bridge/internal/tokenizer"
	"cdx.cc/claude-bridge/internal/transform"
	"cdx.cc/claude-bridge/internal/types"
	"cdx.cc/claude-bridge/internal/upstream"

	"github.com/go-chi/chi/v5"
)

const (
	defaultStoreTTL                  = 30 * time.Minute
	bestPracticePromptCacheRetention = "24h"
)

type Server struct {
	cfg      config.Config
	rtCfg    *config.RuntimeConfig
	log      *slog.Logger
	upstream *upstream.Client
	store    store.Store
	mode     config.Mode
}

func New(cfg config.Config, rtCfg *config.RuntimeConfig, logger *slog.Logger) (*Server, error) {
	var st store.Store
	if cfg.RedisURL != "" {
		redisStore, err := store.NewRedisStore(cfg.RedisURL, defaultStoreTTL)
		if err != nil {
			return nil, err
		}
		st = redisStore
	} else {
		st = store.NewMemoryStore()
	}

	return &Server{
		cfg:      cfg,
		rtCfg:    rtCfg,
		log:      logger,
		upstream: upstream.NewDynamicClient(rtCfg, cfg.Timeout),
		store:    st,
		mode:     cfg.Mode,
	}, nil
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Get("/health", s.handleHealth)
	r.Get("/v1/models", s.handleModels)
	r.Post("/v1/messages", s.handleMessages)
	r.Post("/v1/messages/count_tokens", s.handleCountTokens)
	r.Post("/v1/responses/compact", s.handleResponsesCompact)
	r.Post("/responses/compact", s.handleResponsesCompact)

	// Claude Code /fast 模式需要此端点返回 enabled 状态
	r.Get("/api/claude_code_penguin_mode", s.handlePenguinMode)

	// 注册管理面板路由
	adminHandler := admin.NewHandler(s.rtCfg, s.upstream, s.log)
	adminHandler.Register(r)

	return r
}

func (s *Server) Close() error {
	if s.store != nil {
		return s.store.Close()
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleCountTokens 估算 input token 数量
// CC 依赖此端点管理上下文窗口、/context 命令和 cost 计算
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if !s.validateAuth(r) {
		s.writeError(w, http.StatusUnauthorized, errors.New("invalid or missing auth token"))
		return
	}

	body, err := s.readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}

	// 用 tiktoken 精确计算 input token 数
	estimated := tokenizer.CountRequestBody(body)
	if req, err := transform.DecodeAnthropicRequest(body, s.mode); err == nil {
		modelMap := s.rtCfg.GetModelMap()
		if oaReq, err := transform.TransformAnthropicToOpenAI(req, s.mode, modelMap); err == nil {
			_, estimated = s.prepareAutoCompact(r.Context(), r, oaReq, estimated)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimated})
}

// handleModels 返回已映射的入站模型列表（Anthropic /v1/models 格式）
// bridge 入站是 Anthropic 协议，第三方客户端用 Claude API 兼容模式接入
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	modelMap := s.rtCfg.GetModelMap()

	type modelEntry struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		CreatedAt   string `json:"created_at"`
	}

	now := time.Now().UTC().Format(time.RFC3339)
	data := make([]modelEntry, 0, len(modelMap))
	var firstID, lastID string
	for name := range modelMap {
		if firstID == "" {
			firstID = name
		}
		lastID = name
		data = append(data, modelEntry{
			Type:        "model",
			ID:          name,
			DisplayName: name,
			CreatedAt:   now,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data":     data,
		"has_more": false,
		"first_id": firstID,
		"last_id":  lastID,
	})
}

func (s *Server) handlePenguinMode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"enabled":true,"disabled_reason":null}`))
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	// 验证入站 Auth Token
	if !s.validateAuth(r) {
		s.writeError(w, http.StatusUnauthorized, errors.New("invalid or missing auth token"))
		return
	}

	ctx := r.Context()
	body, err := s.readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}
	estimatedInputTokens := tokenizer.CountRequestBody(body)

	req, err := transform.DecodeAnthropicRequest(body, s.mode)
	if err != nil {
		s.log.Error("decode request failed", slog.Any("err", err))
		s.writeError(w, http.StatusBadRequest, err)
		return
	}

	// 从 RuntimeConfig 动态读取模型映射
	modelMap := s.rtCfg.GetModelMap()
	oaReq, err := transform.TransformAnthropicToOpenAI(req, s.mode, modelMap)
	if err != nil {
		s.log.Error("transform request failed", slog.Any("err", err))
		s.writeError(w, http.StatusBadRequest, err)
		return
	}
	oaReq, estimatedInputTokens = s.prepareAutoCompact(ctx, r, oaReq, estimatedInputTokens)
	s.applyPromptCachePolicy(&oaReq, req, r)

	s.log.Debug("outgoing request",
		slog.String("model", oaReq.Model),
		slog.Bool("stream", oaReq.Stream),
		slog.Int("input_items", len(oaReq.Input)),
		slog.Int("tools", len(oaReq.Tools)),
		slog.Int("instructions_len", len(oaReq.Instructions)),
		slog.Bool("has_reasoning", oaReq.Reasoning != nil),
	)

	if req.Stream {
		s.handleStream(ctx, w, r, oaReq, req.Model, estimatedInputTokens)
		return
	}

	s.handleNonStream(ctx, w, r, oaReq, req.Model)
}

// handleResponsesCompact 透传 OpenAI/Codex 远程 compaction 端点。
// 这和客户端本地 /compact 命令不同：/compact 是 CLI 命令，可能最终触发远程 compact API。
func (s *Server) handleResponsesCompact(w http.ResponseWriter, r *http.Request) {
	if !s.validateAuth(r) {
		s.writeError(w, http.StatusUnauthorized, errors.New("invalid or missing auth token"))
		return
	}

	body, err := s.readBody(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}

	body = s.rewriteOpenAIRequestBody(body, r)
	headers := s.forwardPassthroughHeaders(r)
	var (
		resp *http.Response
		data []byte
	)
	for attempts := 0; attempts < 3; attempts++ {
		resp, data, err = s.upstream.DoJSON(r.Context(), "/v1/responses/compact", json.RawMessage(body), headers)
		if err != nil {
			s.writeErrorWithType(w, http.StatusBadGateway, "api_error", err.Error(), "")
			return
		}
		var stripped bool
		body, stripped = stripUnsupportedPromptCacheFieldsFromBody(resp.StatusCode, data, body)
		if stripped {
			continue
		}
		break
	}

	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	if reqID := upstreamRequestID(resp.Header); reqID != "" {
		w.Header().Set("request-id", reqID)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}

func (s *Server) handleNonStream(ctx context.Context, w http.ResponseWriter, r *http.Request, oaReq types.OpenAIResponsesRequest, requestModel string) {
	headers := s.forwardHeaders(r)
	var (
		resp *http.Response
		data []byte
		err  error
	)
	for attempts := 0; attempts < 3; attempts++ {
		resp, data, err = s.upstream.DoJSON(ctx, "/v1/responses", oaReq, headers)
		if err != nil {
			s.writeErrorWithType(w, http.StatusBadGateway, "api_error", err.Error(), "")
			return
		}
		var stripped bool
		oaReq, stripped = stripUnsupportedPromptCacheFieldsFromRequest(resp.StatusCode, data, oaReq)
		if !stripped {
			oaReq, stripped = stripReasoningOnAccountExhaustion(resp.StatusCode, data, oaReq)
		}
		if stripped {
			continue
		}
		break
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.writeUpstreamError(w, resp.StatusCode, data, upstreamRequestID(resp.Header))
		return
	}

	var oaResp types.OpenAIResponse
	if err := json.Unmarshal(data, &oaResp); err != nil {
		s.writeErrorWithType(w, http.StatusBadGateway, "api_error", err.Error(), upstreamRequestID(resp.Header))
		return
	}

	anthResp, err := transform.TransformOpenAIToAnthropic(oaResp, s.mode, requestModel)
	if err != nil {
		s.writeErrorWithType(w, http.StatusBadGateway, "api_error", err.Error(), upstreamRequestID(resp.Header))
		return
	}

	// 从上游 usage 提取 token 数，设置 Anthropic 兼容头
	inputTokens, outputTokens := 0, 0
	if oaResp.Usage != nil {
		inputTokens = oaResp.Usage.InputTokens
		outputTokens = oaResp.Usage.OutputTokens
	}
	cacheReadTokens := 0
	if oaResp.Usage != nil && oaResp.Usage.InputTokenDetails != nil {
		cacheReadTokens = oaResp.Usage.InputTokenDetails.CachedTokens
	}
	anthResp.Usage.ServiceTier = transformServiceTier(oaReq.ServiceTier)
	anthResp.Usage.Speed = transformSpeed(oaReq.ServiceTier)
	s.backfillCacheUsage(&anthResp.Usage, cacheReadTokens, oaReq.PromptCacheRetention)
	if anthResp.Usage.InputTokens > 0 || anthResp.Usage.OutputTokens > 0 {
		anthResp.Usage.Iterations = []types.AnthropicUsageIteration{{
			InputTokens:  anthResp.Usage.InputTokens,
			OutputTokens: anthResp.Usage.OutputTokens,
		}}
	}
	s.setAnthropicHeaders(w, inputTokens, outputTokens, 0, cacheReadTokens, upstreamRequestID(resp.Header))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(anthResp); err != nil {
		s.log.Error("write response", slog.Any("err", err))
	}
}

func (s *Server) handleStream(ctx context.Context, w http.ResponseWriter, r *http.Request, oaReq types.OpenAIResponsesRequest, requestModel string, estimatedInputTokens int) {
	headers := s.forwardHeaders(r)
	oaReq.Stream = true

	var (
		resp *http.Response
		err  error
	)
	for attempts := 0; attempts < 3; attempts++ {
		resp, err = s.upstream.DoStream(ctx, "/v1/responses", oaReq, headers)
		if err == nil {
			break
		}
		var stripped bool
		if resp != nil {
			oaReq, stripped = stripUnsupportedPromptCacheFieldsFromStreamError(resp.StatusCode, err.Error(), oaReq)
			if !stripped {
				oaReq, stripped = stripReasoningOnAccountExhaustionFromErr(resp.StatusCode, err.Error(), oaReq)
			}
		}
		if stripped {
			continue
		}
	}
	if err != nil {
		s.log.Error("upstream DoStream failed", slog.Any("err", err))
		if resp != nil && resp.StatusCode >= 400 {
			s.writeErrorWithType(w, resp.StatusCode, anthropicErrorTypeForStatus(resp.StatusCode, "", ""), err.Error(), upstreamRequestID(resp.Header))
		} else {
			s.writeErrorWithType(w, http.StatusBadGateway, "api_error", err.Error(), "")
		}
		return
	}
	s.log.Debug("upstream stream connected", slog.Int("status", resp.StatusCode))

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, errors.New("streaming not supported"))
		return
	}

	// 流式模式：header 必须在第一次 write 前设置
	s.setAnthropicHeaders(w, estimatedInputTokens, 0, 0, 0, upstreamRequestID(resp.Header))

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	writer := sse.NewWriter(w, flusher.Flush)
	events, errs := sse.Read(resp.Body)
	defer resp.Body.Close()

	bridgeErr := transform.BridgeOpenAIStream(ctx, events, writer, s.mode, requestModel, estimatedInputTokens, oaReq.ServiceTier, oaReq.PromptCacheRetention)
	s.log.Debug("stream bridge finished", slog.Any("err", bridgeErr))
	if bridgeErr != nil && !errors.Is(bridgeErr, context.Canceled) {
		s.log.Error("stream bridge error", slog.Any("err", bridgeErr))
		_ = writer.Send("error", mustJSON(types.AnthropicError{
			Type: "error",
			Error: types.AnthropicErrorBody{
				Type:    "stream_error",
				Message: bridgeErr.Error(),
			},
		}))
	}

	select {
	case err := <-errs:
		if err != nil {
			s.log.Error("upstream stream error", slog.Any("err", err))
		}
	default:
	}
}

func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	reader := http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	defer r.Body.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("empty request body")
	}
	return data, nil
}

func (s *Server) forwardHeaders(r *http.Request) map[string]string {
	headers := make(map[string]string)
	// 上游已配置 API Key → 使用自己的 key，不转发客户端 header
	if s.upstream.HasAPIKey() {
		return headers
	}
	// bridge 配置了 auth token → 客户端 header 是 bridge 自己的 token，不应转发
	if s.rtCfg.GetAuthToken() != "" {
		return headers
	}
	// 透传模式：bridge 无 auth token 也无 upstream key → 转发客户端凭证
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth != "" {
		headers["Authorization"] = auth
		return headers
	}
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		if strings.HasPrefix(strings.ToLower(key), "bearer ") {
			headers["Authorization"] = key
		} else {
			headers["Authorization"] = "Bearer " + key
		}
	}
	return headers
}

func (s *Server) forwardPassthroughHeaders(r *http.Request) map[string]string {
	headers := make(map[string]string)
	for key, values := range r.Header {
		if len(values) == 0 || skipPassthroughHeader(key) {
			continue
		}
		headers[key] = strings.Join(values, ", ")
	}

	// bridge 使用自己的上游 key 或自己的入站 token 时，不把客户端凭证继续透传到上游
	if s.upstream.HasAPIKey() || s.rtCfg.GetAuthToken() != "" {
		delete(headers, "Authorization")
		delete(headers, "X-Api-Key")
		delete(headers, "X-API-Key")
	}
	return headers
}

func skipPassthroughHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "host", "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "content-length":
		return true
	default:
		return false
	}
}

func (s *Server) rewriteOpenAIRequestBody(body []byte, r *http.Request) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	if modelRaw, ok := raw["model"]; ok {
		var model string
		if err := json.Unmarshal(modelRaw, &model); err == nil {
			model = strings.TrimSpace(model)
			if model != "" {
				modelMap := s.rtCfg.GetModelMap()
				if mapping, ok := modelMap[model]; ok && strings.TrimSpace(mapping.UpstreamModel) != "" && mapping.UpstreamModel != model {
					if encoded, err := json.Marshal(mapping.UpstreamModel); err == nil {
						raw["model"] = encoded
					}
				}
			}
		}
	}
	promptCacheCfg := s.rtCfg.GetPromptCache()
	existingRetention := extractPromptCacheRetention(body)
	existingKey := extractPromptCacheKey(body)
	derivedKey := ""
	if promptCacheCfg.AutoKey && existingKey == "" {
		derivedKey = derivePromptCacheKeyFromRawRequest(raw, r)
	}
	if promptCacheCfg.AutoKey && existingKey == "" && derivedKey != "" {
		raw["prompt_cache_key"] = mustJSON(derivedKey)
	}
	if existingRetention == "" && shouldInjectPromptCacheRetention(promptCacheCfg, existingKey, derivedKey, false) {
		raw["prompt_cache_retention"] = mustJSON(bestPracticePromptCacheRetention)
	}
	rewritten, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return rewritten
}

func (s *Server) applyPromptCachePolicy(oaReq *types.OpenAIResponsesRequest, req types.AnthropicMessageRequest, r *http.Request) {
	promptCacheCfg := s.rtCfg.GetPromptCache()
	derivedKey := ""
	if promptCacheCfg.AutoKey && strings.TrimSpace(oaReq.PromptCacheKey) == "" {
		derivedKey = derivePromptCacheKey(req.Metadata, r)
		if derivedKey != "" {
			oaReq.PromptCacheKey = derivedKey
		}
	}
	if strings.TrimSpace(oaReq.PromptCacheRetention) == "" && shouldInjectPromptCacheRetention(promptCacheCfg, oaReq.PromptCacheKey, derivedKey, anthropicRequestHasCacheControl(req)) {
		oaReq.PromptCacheRetention = bestPracticePromptCacheRetention
	}
}

func shouldInjectPromptCacheRetention(cfg config.PromptCacheConfig, existingKey, derivedKey string, hasAnthropicCacheControl bool) bool {
	switch cfg.Mode {
	case config.PromptCacheOff:
		return false
	case config.PromptCacheForce24H:
		return true
	case config.PromptCacheAuto:
		return strings.TrimSpace(existingKey) != "" || strings.TrimSpace(derivedKey) != "" || hasAnthropicCacheControl
	default:
		return true
	}
}

func anthropicRequestHasCacheControl(req types.AnthropicMessageRequest) bool {
	if systemHasCacheControl(req.System) {
		return true
	}
	for _, msg := range req.Messages {
		blocks, err := transformParseContentBlocks(msg.Content)
		if err != nil {
			continue
		}
		for _, block := range blocks {
			if _, ok := block.ExtraFields["cache_control"]; ok {
				return true
			}
		}
	}
	return false
}

func systemHasCacheControl(raw json.RawMessage) bool {
	if len(raw) == 0 || raw[0] != '[' {
		return false
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return false
	}
	for _, block := range blocks {
		if _, ok := block["cache_control"]; ok {
			return true
		}
	}
	return false
}

func transformParseContentBlocks(raw json.RawMessage) ([]types.AnthropicContentBlock, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, err
		}
		return []types.AnthropicContentBlock{{Type: "text", Text: text}}, nil
	}
	var blocks []types.AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

func derivePromptCacheKeyFromRawRequest(raw map[string]json.RawMessage, r *http.Request) string {
	var metadata map[string]any
	if val, ok := raw["metadata"]; ok && len(val) > 0 {
		_ = json.Unmarshal(val, &metadata)
	}
	return derivePromptCacheKey(metadata, r)
}

func derivePromptCacheKey(metadata map[string]any, r *http.Request) string {
	accountUUID, deviceID, sessionID := extractPromptCacheIdentity(metadata)
	var parts []string
	if accountUUID != "" {
		parts = append(parts, "acct:"+accountUUID)
	}
	if deviceID != "" {
		parts = append(parts, "dev:"+deviceID)
	}
	if accountUUID == "" && deviceID == "" && sessionID != "" {
		parts = append(parts, "sess:"+sessionID)
	}
	if auth := incomingAuthValue(r); auth != "" {
		parts = append(parts, "auth:"+auth)
	}
	if ua := strings.TrimSpace(r.UserAgent()); ua != "" {
		parts = append(parts, "ua:"+ua)
	}
	if len(parts) == 0 {
		return ""
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return "cc-pc-" + hex.EncodeToString(digest[:16])
}

func extractPromptCacheIdentity(metadata map[string]any) (accountUUID, deviceID, sessionID string) {
	if len(metadata) == 0 {
		return "", "", ""
	}
	consume := func(source map[string]any) {
		if accountUUID == "" {
			accountUUID = stringFromAny(source["account_uuid"])
		}
		if deviceID == "" {
			deviceID = stringFromAny(source["device_id"])
		}
		if sessionID == "" {
			sessionID = stringFromAny(source["session_id"])
		}
	}
	consume(metadata)
	if rawUserID := metadata["user_id"]; rawUserID != nil {
		switch val := rawUserID.(type) {
		case string:
			var parsed map[string]any
			if err := json.Unmarshal([]byte(val), &parsed); err == nil {
				consume(parsed)
			}
		case map[string]any:
			consume(val)
		}
	}
	return
}

func stringFromAny(value any) string {
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func incomingAuthValue(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, key := range []string{"Authorization", "X-API-Key"} {
		value := strings.TrimSpace(r.Header.Get(key))
		if value == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(value), "bearer ") {
			value = strings.TrimSpace(value[7:])
		}
		if value != "" {
			return value
		}
	}
	return ""
}

func shouldRetryWithoutPromptCacheRetention(status int, body []byte, retention string) bool {
	if strings.TrimSpace(retention) == "" {
		return false
	}
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity && status != http.StatusNotImplemented {
		return false
	}
	return promptCacheRetentionUnsupported(strings.ToLower(strings.TrimSpace(string(body))))
}

func shouldRetryWithoutPromptCacheRetentionFromErr(status int, errText, retention string) bool {
	if strings.TrimSpace(retention) == "" {
		return false
	}
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity && status != http.StatusNotImplemented {
		return false
	}
	return promptCacheRetentionUnsupported(strings.ToLower(strings.TrimSpace(errText)))
}

func shouldRetryWithoutPromptCacheKey(status int, body []byte, key string) bool {
	if strings.TrimSpace(key) == "" {
		return false
	}
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity && status != http.StatusNotImplemented {
		return false
	}
	return promptCacheKeyUnsupported(strings.ToLower(strings.TrimSpace(string(body))))
}

func shouldRetryWithoutPromptCacheKeyFromErr(status int, errText, key string) bool {
	if strings.TrimSpace(key) == "" {
		return false
	}
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity && status != http.StatusNotImplemented {
		return false
	}
	return promptCacheKeyUnsupported(strings.ToLower(strings.TrimSpace(errText)))
}

func promptCacheRetentionUnsupported(text string) bool {
	if !strings.Contains(text, "prompt_cache_retention") {
		return false
	}
	return strings.Contains(text, "not supported") ||
		strings.Contains(text, "unsupported") ||
		strings.Contains(text, "unknown") ||
		strings.Contains(text, "invalid")
}

func promptCacheKeyUnsupported(text string) bool {
	if !strings.Contains(text, "prompt_cache_key") {
		return false
	}
	return strings.Contains(text, "not supported") ||
		strings.Contains(text, "unsupported") ||
		strings.Contains(text, "unknown") ||
		strings.Contains(text, "invalid")
}

func shouldRetryWithoutPromptCacheOnOpaqueUpstreamFailure(status int, text string, hasKey, hasRetention bool) bool {
	if (!hasKey && !hasRetention) || status != http.StatusBadGateway {
		return false
	}
	text = strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(text, "upstream_error") || strings.Contains(text, "upstream request failed")
}

func shouldRetryWithoutReasoningOnAccountExhaustion(status int, text string, reasoning *types.OpenAIReasoning) bool {
	if reasoning == nil {
		return false
	}
	if status != http.StatusBadGateway && status != http.StatusServiceUnavailable {
		return false
	}
	text = strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(text, "account") || strings.Contains(text, "exhaust")
}

func extractPromptCacheRetention(body []byte) string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	value, ok := raw["prompt_cache_retention"]
	if !ok {
		return ""
	}
	var retention string
	if err := json.Unmarshal(value, &retention); err != nil {
		return ""
	}
	return strings.TrimSpace(retention)
}

func extractPromptCacheKey(body []byte) string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	value, ok := raw["prompt_cache_key"]
	if !ok {
		return ""
	}
	var key string
	if err := json.Unmarshal(value, &key); err != nil {
		return ""
	}
	return strings.TrimSpace(key)
}

func clearPromptCacheKey(body []byte) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	delete(raw, "prompt_cache_key")
	data, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return data
}

func clearPromptCacheRetention(body []byte) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	delete(raw, "prompt_cache_retention")
	data, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return data
}

func stripUnsupportedPromptCacheFieldsFromRequest(status int, body []byte, req types.OpenAIResponsesRequest) (types.OpenAIResponsesRequest, bool) {
	var stripped bool
	if shouldRetryWithoutPromptCacheOnOpaqueUpstreamFailure(status, string(body), strings.TrimSpace(req.PromptCacheKey) != "", strings.TrimSpace(req.PromptCacheRetention) != "") {
		req.PromptCacheKey = ""
		req.PromptCacheRetention = ""
		return req, true
	}
	if shouldRetryWithoutPromptCacheKey(status, body, req.PromptCacheKey) {
		req.PromptCacheKey = ""
		stripped = true
	}
	if shouldRetryWithoutPromptCacheRetention(status, body, req.PromptCacheRetention) {
		req.PromptCacheRetention = ""
		stripped = true
	}
	return req, stripped
}

func stripUnsupportedPromptCacheFieldsFromStreamError(status int, errText string, req types.OpenAIResponsesRequest) (types.OpenAIResponsesRequest, bool) {
	var stripped bool
	if shouldRetryWithoutPromptCacheOnOpaqueUpstreamFailure(status, errText, strings.TrimSpace(req.PromptCacheKey) != "", strings.TrimSpace(req.PromptCacheRetention) != "") {
		req.PromptCacheKey = ""
		req.PromptCacheRetention = ""
		return req, true
	}
	if shouldRetryWithoutPromptCacheKeyFromErr(status, errText, req.PromptCacheKey) {
		req.PromptCacheKey = ""
		stripped = true
	}
	if shouldRetryWithoutPromptCacheRetentionFromErr(status, errText, req.PromptCacheRetention) {
		req.PromptCacheRetention = ""
		stripped = true
	}
	return req, stripped
}

func stripUnsupportedPromptCacheFieldsFromBody(status int, responseBody, requestBody []byte) ([]byte, bool) {
	var stripped bool
	if shouldRetryWithoutPromptCacheOnOpaqueUpstreamFailure(status, string(responseBody), strings.TrimSpace(extractPromptCacheKey(requestBody)) != "", strings.TrimSpace(extractPromptCacheRetention(requestBody)) != "") {
		requestBody = clearPromptCacheKey(requestBody)
		requestBody = clearPromptCacheRetention(requestBody)
		return requestBody, true
	}
	if shouldRetryWithoutPromptCacheKey(status, responseBody, extractPromptCacheKey(requestBody)) {
		requestBody = clearPromptCacheKey(requestBody)
		stripped = true
	}
	if shouldRetryWithoutPromptCacheRetention(status, responseBody, extractPromptCacheRetention(requestBody)) {
		requestBody = clearPromptCacheRetention(requestBody)
		stripped = true
	}
	return requestBody, stripped
}

func stripReasoningOnAccountExhaustion(status int, body []byte, req types.OpenAIResponsesRequest) (types.OpenAIResponsesRequest, bool) {
	if shouldRetryWithoutReasoningOnAccountExhaustion(status, string(body), req.Reasoning) {
		req.Reasoning = nil
		return req, true
	}
	return req, false
}

func stripReasoningOnAccountExhaustionFromErr(status int, errText string, req types.OpenAIResponsesRequest) (types.OpenAIResponsesRequest, bool) {
	if shouldRetryWithoutReasoningOnAccountExhaustion(status, errText, req.Reasoning) {
		req.Reasoning = nil
		return req, true
	}
	return req, false
}

func (s *Server) backfillCacheUsage(usage *types.AnthropicUsage, cachedTokens int, retention string) {
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

// validateAuth 验证入站请求的 Auth Token
func (s *Server) validateAuth(r *http.Request) bool {
	token := s.rtCfg.GetAuthToken()
	if token == "" {
		return true // 未配置 Token 则跳过验证
	}

	auth := r.Header.Get("Authorization")
	if auth == "" {
		auth = r.Header.Get("X-API-Key")
	}
	// 去除 Bearer 前缀
	auth = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer"))
	auth = strings.TrimSpace(auth)

	return auth == token
}

// setAnthropicHeaders 设置 Anthropic 兼容的 rate limit 和 request-id 响应头
// Claude Code 依赖这些头来显示状态栏的 token 计数和上下文百分比
func (s *Server) setAnthropicHeaders(w http.ResponseWriter, inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int, requestID string) {
	contextLimit := s.cfg.ContextLimit
	outputLimit := s.cfg.OutputLimit
	tokensLimit := contextLimit + outputLimit
	contextUsed := inputTokens + cacheCreationTokens + cacheReadTokens
	tokensUsed := contextUsed + outputTokens
	tokensRemaining := tokensLimit - tokensUsed
	if tokensRemaining < 0 {
		tokensRemaining = 0
	}
	inputRemaining := contextLimit - contextUsed
	if inputRemaining < 0 {
		inputRemaining = 0
	}
	outputRemaining := outputLimit - outputTokens
	if outputRemaining < 0 {
		outputRemaining = 0
	}

	now := time.Now()
	reset := now.Add(60 * time.Second).UTC().Format(time.RFC3339)
	unified5hReset := now.Add(5 * time.Hour).Unix()
	unified7dReset := now.Add(7 * 24 * time.Hour).Unix()

	w.Header().Set("anthropic-ratelimit-tokens-limit", fmt.Sprintf("%d", tokensLimit))
	w.Header().Set("anthropic-ratelimit-tokens-remaining", fmt.Sprintf("%d", tokensRemaining))
	w.Header().Set("anthropic-ratelimit-tokens-reset", reset)
	w.Header().Set("anthropic-ratelimit-input-tokens-limit", fmt.Sprintf("%d", contextLimit))
	w.Header().Set("anthropic-ratelimit-input-tokens-remaining", fmt.Sprintf("%d", inputRemaining))
	w.Header().Set("anthropic-ratelimit-input-tokens-reset", reset)
	w.Header().Set("anthropic-ratelimit-output-tokens-limit", fmt.Sprintf("%d", outputLimit))
	w.Header().Set("anthropic-ratelimit-output-tokens-remaining", fmt.Sprintf("%d", outputRemaining))
	w.Header().Set("anthropic-ratelimit-output-tokens-reset", reset)
	w.Header().Set("anthropic-ratelimit-requests-limit", "4000")
	w.Header().Set("anthropic-ratelimit-requests-remaining", "3999")
	w.Header().Set("anthropic-ratelimit-requests-reset", reset)
	utilization := utilizationPercent(tokensUsed, tokensLimit)
	unifiedStatus := "allowed"
	if tokensRemaining == 0 {
		unifiedStatus = "exhausted"
	}
	overageStatus := "allowed"
	if tokensUsed > tokensLimit {
		overageStatus = "blocked"
	}
	w.Header().Set("anthropic-ratelimit-unified-status", unifiedStatus)
	w.Header().Set("anthropic-ratelimit-unified-reset", fmt.Sprintf("%d", unified5hReset))
	w.Header().Set("anthropic-ratelimit-unified-fallback", "unavailable")
	w.Header().Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	w.Header().Set("anthropic-ratelimit-unified-overage-status", overageStatus)
	w.Header().Set("anthropic-ratelimit-unified-overage-reset", fmt.Sprintf("%d", unified5hReset))
	w.Header().Set("anthropic-ratelimit-unified-5h-utilization", fmt.Sprintf("%d", utilization))
	w.Header().Set("anthropic-ratelimit-unified-5h-reset", fmt.Sprintf("%d", unified5hReset))
	w.Header().Set("anthropic-ratelimit-unified-7d-utilization", fmt.Sprintf("%d", utilization))
	w.Header().Set("anthropic-ratelimit-unified-7d-reset", fmt.Sprintf("%d", unified7dReset))
	if strings.TrimSpace(requestID) == "" {
		requestID = "req_bridge_" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	w.Header().Set("request-id", requestID)
}

func utilizationPercent(used, limit int) int {
	if limit <= 0 || used <= 0 {
		return 0
	}
	value := used * 100 / limit
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func upstreamRequestID(header http.Header) string {
	for _, key := range []string{"request-id", "x-request-id", "openai-request-id"} {
		if value := strings.TrimSpace(header.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func transformServiceTier(serviceTier string) string {
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

func transformSpeed(serviceTier string) string {
	switch strings.TrimSpace(strings.ToLower(serviceTier)) {
	case "priority":
		return "fast"
	default:
		return "standard"
	}
}

func (s *Server) writeUpstreamError(w http.ResponseWriter, status int, body []byte, requestID string) {
	message, upstreamType, upstreamCode := parseUpstreamErrorBody(body)
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(status)
	}
	s.writeErrorWithType(w, status, anthropicErrorTypeForStatus(status, upstreamType, upstreamCode), message, requestID)
}

func anthropicErrorTypeForStatus(status int, upstreamType, upstreamCode string) string {
	lowered := strings.ToLower(strings.TrimSpace(upstreamType + " " + upstreamCode))
	switch {
	case strings.Contains(lowered, "rate_limit"), strings.Contains(lowered, "quota"), status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case strings.Contains(lowered, "auth"), strings.Contains(lowered, "api_key"), status == http.StatusUnauthorized:
		return "authentication_error"
	case strings.Contains(lowered, "permission"), status == http.StatusForbidden:
		return "permission_error"
	case strings.Contains(lowered, "not_found"), status == http.StatusNotFound:
		return "not_found_error"
	case strings.Contains(lowered, "overloaded"), strings.Contains(lowered, "timeout"), status == http.StatusGatewayTimeout, status == http.StatusBadGateway, status == http.StatusServiceUnavailable:
		return "api_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

func parseUpstreamErrorBody(body []byte) (message, errType, code string) {
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", ""
	}
	message = strings.TrimSpace(payload.Error.Message)
	if message == "" {
		message = strings.TrimSpace(payload.Message)
	}
	errType = strings.TrimSpace(payload.Error.Type)
	if errType == "" {
		errType = strings.TrimSpace(payload.Type)
	}
	code = strings.TrimSpace(payload.Error.Code)
	if code == "" {
		code = strings.TrimSpace(payload.Code)
	}
	return
}

func (s *Server) writeError(w http.ResponseWriter, status int, err error) {
	s.writeErrorWithType(w, status, "invalid_request_error", err.Error(), "")
}

func (s *Server) writeErrorWithType(w http.ResponseWriter, status int, errType, message, requestID string) {
	payload := types.AnthropicError{
		Type: "error",
		Error: types.AnthropicErrorBody{
			Type:    firstNonEmptyErrorType(errType),
			Message: message,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	if strings.TrimSpace(requestID) != "" {
		w.Header().Set("request-id", requestID)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func firstNonEmptyErrorType(value string) string {
	if strings.TrimSpace(value) == "" {
		return "invalid_request_error"
	}
	return value
}

func mustJSON(val any) []byte {
	data, _ := json.Marshal(val)
	return data
}
