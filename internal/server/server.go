package server

import (
	"context"
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
	"cdx.cc/claude-bridge/internal/transform"
	"cdx.cc/claude-bridge/internal/types"
	"cdx.cc/claude-bridge/internal/upstream"

	"github.com/go-chi/chi/v5"
)

const (
	defaultStoreTTL = 30 * time.Minute
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
	r.Post("/v1/messages", s.handleMessages)
	r.Post("/v1/messages/count_tokens", s.handleCountTokens)

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

	// 估算 token 数：JSON 字节数 / 4（英文约 4 字符/token，中文约 2 字符/token，取中间值）
	estimated := len(body) / 3
	if estimated < 1 {
		estimated = 1
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimated})
}

func (s *Server) handlePenguinMode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"enabled":true}`))
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

	s.log.Debug("outgoing request",
		slog.String("model", oaReq.Model),
		slog.Bool("stream", oaReq.Stream),
		slog.Int("input_items", len(oaReq.Input)),
		slog.Int("tools", len(oaReq.Tools)),
		slog.Int("instructions_len", len(oaReq.Instructions)),
		slog.Bool("has_reasoning", oaReq.Reasoning != nil),
	)

	if req.Stream {
		s.handleStream(ctx, w, r, oaReq, req.Model, len(body))
		return
	}

	s.handleNonStream(ctx, w, r, oaReq, req.Model)
}

func (s *Server) handleNonStream(ctx context.Context, w http.ResponseWriter, r *http.Request, oaReq types.OpenAIResponsesRequest, requestModel string) {
	headers := s.forwardHeaders(r)
	resp, data, err := s.upstream.DoJSON(ctx, "/v1/responses", oaReq, headers)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.writeError(w, resp.StatusCode, errors.New(string(data)))
		return
	}

	var oaResp types.OpenAIResponse
	if err := json.Unmarshal(data, &oaResp); err != nil {
		s.writeError(w, http.StatusBadGateway, err)
		return
	}

	anthResp, err := transform.TransformOpenAIToAnthropic(oaResp, s.mode, requestModel)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err)
		return
	}

	// 从上游 usage 提取 token 数，设置 Anthropic 兼容头
	inputTokens, outputTokens := 0, 0
	if oaResp.Usage != nil {
		inputTokens = oaResp.Usage.InputTokens
		outputTokens = oaResp.Usage.OutputTokens
	}
	s.setAnthropicHeaders(w, inputTokens, outputTokens)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(anthResp); err != nil {
		s.log.Error("write response", slog.Any("err", err))
	}
}

func (s *Server) handleStream(ctx context.Context, w http.ResponseWriter, r *http.Request, oaReq types.OpenAIResponsesRequest, requestModel string, requestBodyLen int) {
	headers := s.forwardHeaders(r)
	oaReq.Stream = true

	resp, err := s.upstream.DoStream(ctx, "/v1/responses", oaReq, headers)
	if err != nil {
		s.log.Error("upstream DoStream failed", slog.Any("err", err))
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	s.log.Debug("upstream stream connected", slog.Int("status", resp.StatusCode))

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, errors.New("streaming not supported"))
		return
	}

	// 流式模式：header 必须在第一次 write 前设置
	// 使用请求体大小估算 input token 数
	estimatedInputTokens := requestBodyLen / 3
	if estimatedInputTokens < 1 {
		estimatedInputTokens = 1
	}
	s.setAnthropicHeaders(w, estimatedInputTokens, 0)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	writer := sse.NewWriter(w, flusher.Flush)
	events, errs := sse.Read(resp.Body)
	defer resp.Body.Close()

	bridgeErr := transform.BridgeOpenAIStream(ctx, events, writer, s.mode, requestModel, estimatedInputTokens)
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
func (s *Server) setAnthropicHeaders(w http.ResponseWriter, inputTokens, outputTokens int) {
	// 上下文窗口大小（1M context window）
	const contextLimit = 1048576
	const outputLimit = 32000
	tokensLimit := contextLimit + outputLimit
	tokensUsed := inputTokens + outputTokens
	tokensRemaining := tokensLimit - tokensUsed
	if tokensRemaining < 0 {
		tokensRemaining = 0
	}
	inputRemaining := contextLimit - inputTokens
	if inputRemaining < 0 {
		inputRemaining = 0
	}
	outputRemaining := outputLimit - outputTokens
	if outputRemaining < 0 {
		outputRemaining = 0
	}

	reset := time.Now().Add(60 * time.Second).UTC().Format(time.RFC3339)

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
	w.Header().Set("request-id", "req_bridge_"+fmt.Sprintf("%d", time.Now().UnixNano()))
}

func (s *Server) writeError(w http.ResponseWriter, status int, err error) {
	payload := types.AnthropicError{
		Type: "error",
		Error: types.AnthropicErrorBody{
			Type:    "invalid_request_error",
			Message: err.Error(),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func mustJSON(val any) []byte {
	data, _ := json.Marshal(val)
	return data
}
