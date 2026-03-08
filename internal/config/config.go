package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

type Mode string

const (
	ModeStrict     Mode = "strict"
	ModeBestEffort Mode = "best_effort"
)

const (
	defaultListenAddr      = ":8787"
	defaultTimeoutSecs     = 120
	defaultMaxBodyMB       = 10
	defaultLogLevel        = "info"
	defaultUpstreamBaseURL = "https://api.openai.com"
	defaultContextLimit    = 1048576 // 1M tokens
	defaultOutputLimit     = 32000
)

// ModelMapping 定义一条模型映射规则
type ModelMapping struct {
	UpstreamModel   string `json:"upstream_model"`             // 实际发给上游的模型名
	ReasoningEffort string `json:"reasoning_effort,omitempty"` // 覆盖推理强度（空 = 不覆盖，由 thinking 参数决定）
}

type Config struct {
	ListenAddr      string
	UpstreamBaseURL string
	UpstreamAPIKey  string
	Mode            Mode
	Timeout         time.Duration
	MaxBodyBytes    int64
	RedisURL        string
	LogLevel        string
	ModelMap        map[string]ModelMapping // 入站模型名 → 映射规则
	ContextLimit    int                     // 上下文窗口大小（token 数）
	OutputLimit     int                     // 最大输出 token 数
}

func Load() (Config, error) {
	cfg := Config{
		ListenAddr:     getenv("LISTEN_ADDR", defaultListenAddr),
		UpstreamAPIKey: os.Getenv("UPSTREAM_API_KEY"),
		RedisURL:       os.Getenv("REDIS_URL"),
		LogLevel:       getenv("LOG_LEVEL", defaultLogLevel),
		Mode:           Mode(getenv("MODE", string(ModeBestEffort))),
	}

	baseURL := strings.TrimSpace(os.Getenv("UPSTREAM_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultUpstreamBaseURL
	}
	cfg.UpstreamBaseURL = strings.TrimRight(baseURL, "/")

	timeoutSecs, err := strconv.Atoi(getenv("UPSTREAM_TIMEOUT_SECS", strconv.Itoa(defaultTimeoutSecs)))
	if err != nil || timeoutSecs <= 0 {
		return Config{}, errors.New("UPSTREAM_TIMEOUT_SECS must be a positive integer")
	}
	cfg.Timeout = time.Duration(timeoutSecs) * time.Second

	maxBodyMB, err := strconv.Atoi(getenv("MAX_BODY_MB", strconv.Itoa(defaultMaxBodyMB)))
	if err != nil || maxBodyMB <= 0 {
		return Config{}, errors.New("MAX_BODY_MB must be a positive integer")
	}
	cfg.MaxBodyBytes = int64(maxBodyMB) * 1024 * 1024

	if cfg.Mode != ModeStrict && cfg.Mode != ModeBestEffort {
		return Config{}, errors.New("MODE must be strict or best_effort")
	}

	cfg.ModelMap = parseModelMap(os.Getenv("MODEL_MAP"))

	// 上下文窗口配置
	cfg.ContextLimit = defaultContextLimit
	if v, err := strconv.Atoi(os.Getenv("CONTEXT_LIMIT")); err == nil && v > 0 {
		cfg.ContextLimit = v
	}
	cfg.OutputLimit = defaultOutputLimit
	if v, err := strconv.Atoi(os.Getenv("OUTPUT_LIMIT")); err == nil && v > 0 {
		cfg.OutputLimit = v
	}

	return cfg, nil
}

// parseModelMap 解析 MODEL_MAP 环境变量
// 格式: 入站模型=上游模型:推理强度,入站模型=上游模型:推理强度,...
// 示例: claude-opus-4-6=gpt-5.4:xhigh,claude-sonnet-4-6=gpt-5.4:high,claude-haiku-4-5-20251001=gpt-5.4:low
// 推理强度可选: none, low, medium, high, xhigh
// 省略推理强度则不覆盖（由请求中的 thinking 参数决定）
func parseModelMap(raw string) map[string]ModelMapping {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	m := make(map[string]ModelMapping)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			continue
		}
		inModel := strings.TrimSpace(parts[0])
		rest := strings.TrimSpace(parts[1])
		if inModel == "" || rest == "" {
			continue
		}
		mapping := ModelMapping{}
		if idx := strings.LastIndex(rest, ":"); idx > 0 {
			mapping.UpstreamModel = strings.TrimSpace(rest[:idx])
			mapping.ReasoningEffort = strings.TrimSpace(rest[idx+1:])
		} else {
			mapping.UpstreamModel = rest
		}
		m[inModel] = mapping
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func getenv(key, fallback string) string {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		return val
	}
	return fallback
}
