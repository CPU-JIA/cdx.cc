package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"cdx.cc/claude-bridge/internal/config"
	"cdx.cc/claude-bridge/internal/upstream"
)

// Handler Admin 面板的 HTTP 处理器
type Handler struct {
	rtCfg     *config.RuntimeConfig
	client    *upstream.Client
	logger    *slog.Logger
	startTime time.Time
}

// NewHandler 创建 Admin 处理器
func NewHandler(rtCfg *config.RuntimeConfig, client *upstream.Client, logger *slog.Logger) *Handler {
	return &Handler{
		rtCfg:     rtCfg,
		client:    client,
		logger:    logger,
		startTime: time.Now(),
	}
}

// Register 注册路由到 mux
func (h *Handler) Register(mux interface {
	Get(pattern string, handlerFn http.HandlerFunc)
	Put(pattern string, handlerFn http.HandlerFunc)
}) {
	mux.Get("/admin", h.servePage)
	mux.Get("/admin/api/config", h.getConfig)
	mux.Put("/admin/api/config", h.updateConfig)
	mux.Get("/admin/api/status", h.getStatus)
	mux.Get("/admin/api/models", h.getModels)
}

// servePage 返回嵌入的 HTML 页面
func (h *Handler) servePage(w http.ResponseWriter, r *http.Request) {
	data, err := htmlFS.ReadFile("admin.html")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// configResponse GET 返回的配置结构（上游 API Key 脱敏，Auth Token 完整返回）
type configResponse struct {
	Upstream  upstreamResponse               `json:"upstream"`
	Models    map[string]config.ModelMapping `json:"models,omitempty"`
	AuthToken string                         `json:"auth_token"`
}

type upstreamResponse struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// configRequest PUT 接收的配置结构
type configRequest struct {
	Upstream  *upstreamRequest               `json:"upstream,omitempty"`
	Models    map[string]config.ModelMapping `json:"models,omitempty"`
	AuthToken *string                        `json:"auth_token,omitempty"` // nil=不修改, ""=清除, "xxx"=设置新值
}

type upstreamRequest struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

func (h *Handler) getConfig(w http.ResponseWriter, r *http.Request) {
	data := h.rtCfg.Get()

	resp := configResponse{
		Upstream: upstreamResponse{
			BaseURL: data.Upstream.BaseURL,
			APIKey:  maskAPIKey(data.Upstream.APIKey),
		},
		Models:    data.Models,
		AuthToken: data.AuthToken,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) updateConfig(w http.ResponseWriter, r *http.Request) {
	var req configRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 读取当前配置作为基准
	current := h.rtCfg.Get()

	// 合并上游配置
	if req.Upstream != nil {
		if req.Upstream.BaseURL != "" {
			current.Upstream.BaseURL = req.Upstream.BaseURL
		}
		// 空字符串 = 不修改（脱敏值场景），非空 = 更新
		if req.Upstream.APIKey != "" {
			current.Upstream.APIKey = req.Upstream.APIKey
		}
	}

	// 合并模型映射（传了 models 字段就整体替换）
	if req.Models != nil {
		current.Models = req.Models
	}

	// 合并 Auth Token（指针：nil=不修改，非nil=设置新值）
	if req.AuthToken != nil {
		current.AuthToken = *req.AuthToken
	}

	if err := h.rtCfg.Update(current); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.logger.Info("config updated via admin panel")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func (h *Handler) getStatus(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(h.startTime)

	resp := map[string]string{
		"status": "running",
		"uptime": formatDuration(uptime),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) getModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	models, err := h.client.ListModels(ctx)
	if err != nil {
		h.logger.Error("failed to list upstream models", slog.Any("err", err))
		http.Error(w, "failed to fetch models: "+err.Error(), http.StatusBadGateway)
		return
	}

	resp := map[string]any{
		"models": models,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// maskAPIKey 脱敏 API Key：显示前3位 + ... + 后4位
func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:3] + "..." + key[len(key)-4:]
}

// formatDuration 格式化运行时间
func formatDuration(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}
