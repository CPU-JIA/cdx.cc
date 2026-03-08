package admin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cdx.cc/claude-bridge/internal/config"
	"cdx.cc/claude-bridge/internal/upstream"
)

const (
	cookieName    = "cdx_session"
	sessionMaxAge = 86400 // 24h
)

// Handler Admin 面板的 HTTP 处理器
type Handler struct {
	rtCfg         *config.RuntimeConfig
	client        *upstream.Client
	logger        *slog.Logger
	startTime     time.Time
	sessionSecret []byte // HMAC 签名密钥，启动时随机生成
}

// NewHandler 创建 Admin 处理器
func NewHandler(rtCfg *config.RuntimeConfig, client *upstream.Client, logger *slog.Logger) *Handler {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic("failed to generate session secret: " + err.Error())
	}
	return &Handler{
		rtCfg:         rtCfg,
		client:        client,
		logger:        logger,
		startTime:     time.Now(),
		sessionSecret: secret,
	}
}

var loginTmpl = template.Must(template.New("login").Parse(loginPageHTML))

// Register 注册路由到 mux
func (h *Handler) Register(mux interface {
	Get(pattern string, handlerFn http.HandlerFunc)
	Post(pattern string, handlerFn http.HandlerFunc)
	Put(pattern string, handlerFn http.HandlerFunc)
}) {
	// 公开路由（无需认证）
	mux.Get("/admin/login", h.handleLoginPage)
	mux.Post("/admin/login", h.handleLoginSubmit)
	mux.Post("/admin/logout", h.handleLogout)

	// 受保护路由
	mux.Get("/admin", h.requireAuth(h.servePage))
	mux.Get("/admin/api/config", h.requireAuth(h.getConfig))
	mux.Put("/admin/api/config", h.requireAuth(h.updateConfig))
	mux.Get("/admin/api/status", h.requireAuth(h.getStatus))
	mux.Get("/admin/api/models", h.requireAuth(h.getModels))
}

// createSessionCookie 生成带 HMAC 签名的 session cookie
// 格式: {expiry_unix}.{hmac_hex}
func (h *Handler) createSessionCookie(token string) *http.Cookie {
	expiry := time.Now().Add(sessionMaxAge * time.Second).Unix()
	expiryStr := strconv.FormatInt(expiry, 10)

	mac := hmac.New(sha256.New, h.sessionSecret)
	mac.Write([]byte(token + "." + expiryStr))
	sig := hex.EncodeToString(mac.Sum(nil))

	return &http.Cookie{
		Name:     cookieName,
		Value:    expiryStr + "." + sig,
		Path:     "/admin",
		MaxAge:   sessionMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

// validateSession 验证 session cookie 的签名和有效期
func (h *Handler) validateSession(r *http.Request) bool {
	token := h.rtCfg.GetAuthToken()
	if token == "" {
		return true // 未配置 token 则跳过认证
	}

	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}

	parts := strings.SplitN(cookie.Value, ".", 2)
	if len(parts) != 2 {
		return false
	}

	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	if time.Now().Unix() > expiry {
		return false
	}

	// 重新计算 HMAC 比对（token 变更后旧 session 自动失效）
	mac := hmac.New(sha256.New, h.sessionSecret)
	mac.Write([]byte(token + "." + parts[0]))
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(parts[1]), []byte(expected))
}

// requireAuth 认证中间件：API 返回 401，页面重定向 302
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.rtCfg.GetAuthToken() == "" || h.validateSession(r) {
			next(w, r)
			return
		}

		if strings.HasPrefix(r.URL.Path, "/admin/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}

		http.Redirect(w, r, "/admin/login", http.StatusFound)
	}
}

// handleLoginPage 渲染登录页
func (h *Handler) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// 未配置 token 或已认证则直接进入管理面板
	if h.rtCfg.GetAuthToken() == "" || h.validateSession(r) {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	h.renderLoginPage(w, "")
}

// handleLoginSubmit 处理登录表单提交
func (h *Handler) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.renderLoginPage(w, "请求格式错误")
		return
	}

	inputToken := r.FormValue("token")
	expectedToken := h.rtCfg.GetAuthToken()

	if subtle.ConstantTimeCompare([]byte(inputToken), []byte(expectedToken)) != 1 {
		time.Sleep(500 * time.Millisecond) // 防暴力破解
		h.renderLoginPage(w, "Token 无效，请重试")
		return
	}

	http.SetCookie(w, h.createSessionCookie(expectedToken))
	http.Redirect(w, r, "/admin", http.StatusFound)
}

// handleLogout 清除 session cookie
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/admin",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}

// renderLoginPage 渲染登录页 HTML
func (h *Handler) renderLoginPage(w http.ResponseWriter, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	loginTmpl.Execute(w, map[string]string{"Error": errMsg})
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
	Upstream   upstreamResponse               `json:"upstream"`
	Models     map[string]config.ModelMapping `json:"models,omitempty"`
	AuthToken  string                         `json:"auth_token"`
	ServiceURL string                         `json:"service_url"`
}

type upstreamResponse struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// configRequest PUT 接收的配置结构
type configRequest struct {
	Upstream   *upstreamRequest               `json:"upstream,omitempty"`
	Models     map[string]config.ModelMapping `json:"models,omitempty"`
	AuthToken  *string                        `json:"auth_token,omitempty"`  // nil=不修改, ""=清除, "xxx"=设置新值
	ServiceURL *string                        `json:"service_url,omitempty"` // nil=不修改
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
		Models:     data.Models,
		AuthToken:  data.AuthToken,
		ServiceURL: data.ServiceURL,
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
		newToken := *req.AuthToken
		// 安全校验：auth_token 不能和上游 API Key 相同
		if newToken != "" && current.Upstream.APIKey != "" && newToken == current.Upstream.APIKey {
			http.Error(w, "auth token 不能和上游 API Key 相同", http.StatusBadRequest)
			return
		}
		current.AuthToken = newToken
	}

	// 合并服务地址
	if req.ServiceURL != nil {
		current.ServiceURL = *req.ServiceURL
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

const loginPageHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>cdx.cc - 登录</title>
<style>
:root{--bg:#f8f9fa;--surface:#fff;--border:#e5e7eb;--text:#1a1a1a;--text-muted:#6b7280;--input-bg:#fff;--btn-border:#d1d5db;--accent:#2563eb;--accent-hover:#1d4ed8;--error:#ef4444;--danger-bg:#fef2f2}
[data-theme="dark"]{--bg:#0d0d0d;--surface:#141414;--border:#262626;--text:#d4d4d4;--text-muted:#666;--input-bg:#1a1a1a;--btn-border:#333;--accent:#da7756;--accent-hover:#c4684a;--error:#f87171;--danger-bg:#1f1515}
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:var(--bg);color:var(--text);display:flex;align-items:center;justify-content:center;min-height:100vh;transition:background .2s,color .2s}
.login-box{background:var(--surface);border:1px solid var(--border);border-radius:12px;padding:40px;width:100%;max-width:380px;transition:background .2s,border-color .2s}
.login-box h1{font-size:24px;font-weight:600;text-align:center;margin-bottom:32px}
.login-box h1 .prompt{color:var(--accent);margin-right:2px}
.login-box input{width:100%;padding:10px 14px;border:1px solid var(--btn-border);border-radius:8px;font-size:14px;outline:none;background:var(--input-bg);color:var(--text);transition:border-color .15s;font-family:inherit}
.login-box input:focus{border-color:var(--accent)}
.login-box button{width:100%;padding:10px;border:none;border-radius:8px;background:var(--accent);color:#fff;font-size:14px;cursor:pointer;margin-top:12px;font-family:inherit;transition:background .15s}
.login-box button:hover{background:var(--accent-hover)}
.error-msg{color:var(--error);font-size:13px;text-align:center;margin-top:12px;padding:8px;background:var(--danger-bg);border-radius:6px}
</style>
</head>
<body>
<div class="login-box">
<h1><span class="prompt">&gt;_</span> cdx.cc</h1>
<form method="POST" action="/admin/login">
<input type="password" name="token" placeholder="Auth Token" autofocus required>
<button type="submit">登录</button>
</form>
{{if .Error}}<div class="error-msg">{{.Error}}</div>{{end}}
</div>
<script>var t=localStorage.getItem("cdx-theme")||"light";document.documentElement.setAttribute("data-theme",t);</script>
</body>
</html>`
