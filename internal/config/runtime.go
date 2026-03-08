package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// RuntimeData 存储运行时可变配置
type RuntimeData struct {
	Upstream      UpstreamConfig          `json:"upstream"`
	Models        map[string]ModelMapping `json:"models,omitempty"`
	AuthToken     string                  `json:"auth_token,omitempty"`     // 客户端 API 连接密钥
	AdminPassword string                  `json:"admin_password,omitempty"` // 管理面板登录密码（独立于 AuthToken）
	ServiceURL    string                  `json:"service_url,omitempty"`    // 对外访问地址（如 https://cdx.cc）
}

// UpstreamConfig 上游服务配置
type UpstreamConfig struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
}

// RuntimeConfig 支持热更新的运行时配置
// 线程安全，所有读写通过 Get/Update 方法
type RuntimeConfig struct {
	mu       sync.RWMutex
	filePath string
	data     RuntimeData
}

// NewRuntimeConfig 从静态配置初始化，若 JSON 文件存在则覆盖
func NewRuntimeConfig(cfg Config, filePath string) (*RuntimeConfig, error) {
	rc := &RuntimeConfig{
		filePath: filePath,
		data: RuntimeData{
			Upstream: UpstreamConfig{
				BaseURL: cfg.UpstreamBaseURL,
				APIKey:  cfg.UpstreamAPIKey,
			},
			Models: cfg.ModelMap,
		},
	}

	// 环境变量 AUTH_TOKEN 优先
	if envToken := os.Getenv("AUTH_TOKEN"); envToken != "" {
		rc.data.AuthToken = envToken
	}

	// JSON 文件存在则覆盖环境变量的值
	if err := rc.loadFromFile(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// 安全校验：AuthToken 为空或与上游 API Key 相同时自动重新生成
	changed := false
	if rc.data.AuthToken == "" ||
		(rc.data.Upstream.APIKey != "" && rc.data.AuthToken == rc.data.Upstream.APIKey) {
		rc.data.AuthToken = generateToken()
		changed = true
	}

	// 管理面板密码：独立于 AuthToken，首次启动自动生成
	if rc.data.AdminPassword == "" || rc.data.AdminPassword == rc.data.AuthToken {
		rc.data.AdminPassword = generatePassword()
		changed = true
	}

	if changed {
		if err := rc.saveToFile(); err != nil {
			return nil, fmt.Errorf("failed to save config: %w", err)
		}
	}

	return rc, nil
}

// Get 返回当前配置快照（深拷贝，调用方可安全使用）
func (rc *RuntimeConfig) Get() RuntimeData {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	snapshot := RuntimeData{
		Upstream:      rc.data.Upstream,
		AuthToken:     rc.data.AuthToken,
		AdminPassword: rc.data.AdminPassword,
		ServiceURL:    rc.data.ServiceURL,
	}
	if rc.data.Models != nil {
		snapshot.Models = make(map[string]ModelMapping, len(rc.data.Models))
		for k, v := range rc.data.Models {
			snapshot.Models[k] = v
		}
	}
	return snapshot
}

// GetModelMap 返回当前模型映射（快捷方法）
func (rc *RuntimeConfig) GetModelMap() map[string]ModelMapping {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	if rc.data.Models == nil {
		return nil
	}
	m := make(map[string]ModelMapping, len(rc.data.Models))
	for k, v := range rc.data.Models {
		m[k] = v
	}
	return m
}

// GetUpstream 返回当前上游 URL 和 API Key
func (rc *RuntimeConfig) GetUpstream() (baseURL, apiKey string) {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.data.Upstream.BaseURL, rc.data.Upstream.APIKey
}

// GetAuthToken 返回当前 Auth Token（客户端 API 连接用）
func (rc *RuntimeConfig) GetAuthToken() string {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.data.AuthToken
}

// GetAdminPassword 返回管理面板登录密码
func (rc *RuntimeConfig) GetAdminPassword() string {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.data.AdminPassword
}

// Update 原子更新配置并持久化到 JSON 文件
func (rc *RuntimeConfig) Update(data RuntimeData) error {
	data.Upstream.BaseURL = strings.TrimRight(strings.TrimSpace(data.Upstream.BaseURL), "/")
	if data.Upstream.BaseURL == "" {
		return errors.New("upstream base URL cannot be empty")
	}
	data.ServiceURL = strings.TrimRight(strings.TrimSpace(data.ServiceURL), "/")

	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.data = data
	return rc.saveToFile()
}

// loadFromFile 从 JSON 文件加载配置（不加锁，仅在构造时调用）
func (rc *RuntimeConfig) loadFromFile() error {
	raw, err := os.ReadFile(rc.filePath)
	if err != nil {
		return err
	}

	var data RuntimeData
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}

	// 仅覆盖非空字段
	if data.Upstream.BaseURL != "" {
		rc.data.Upstream.BaseURL = strings.TrimRight(data.Upstream.BaseURL, "/")
	}
	if data.Upstream.APIKey != "" {
		rc.data.Upstream.APIKey = data.Upstream.APIKey
	}
	if data.Models != nil {
		rc.data.Models = data.Models
	}
	if data.AuthToken != "" {
		rc.data.AuthToken = data.AuthToken
	}
	if data.AdminPassword != "" {
		rc.data.AdminPassword = data.AdminPassword
	}
	if data.ServiceURL != "" {
		rc.data.ServiceURL = strings.TrimRight(data.ServiceURL, "/")
	}

	return nil
}

// saveToFile 持久化配置到 JSON 文件（调用方须持有写锁）
func (rc *RuntimeConfig) saveToFile() error {
	dir := filepath.Dir(rc.filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(rc.data, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(rc.filePath, raw, 0o644)
}

// generateToken 生成随机 Auth Token（sk-cdx.cc- 前缀 + 16 位 hex）
func generateToken() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "sk-cdx.cc-" + fmt.Sprintf("%x", os.Getpid())
	}
	return "sk-cdx.cc-" + hex.EncodeToString(b)
}

// generatePassword 生成随机管理密码（sk-cdx.cc- 前缀 + 12 位 hex）
func generatePassword() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "sk-cdx.cc-" + fmt.Sprintf("%x%x", os.Getpid(), os.Getppid())
	}
	return "sk-cdx.cc-" + hex.EncodeToString(b)
}
