package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"cdx.cc/claude-bridge/internal/config"
)

// Client 上游 HTTP 客户端，支持从 RuntimeConfig 动态读取 URL/Key
type Client struct {
	baseURL string // 静态回退值
	apiKey  string // 静态回退值
	rtCfg   *config.RuntimeConfig
	client  *http.Client
}

// NewClient 创建静态配置的客户端（向后兼容）
func NewClient(baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

// NewDynamicClient 创建从 RuntimeConfig 动态读取配置的客户端
func NewDynamicClient(rtCfg *config.RuntimeConfig, timeout time.Duration) *Client {
	return &Client{
		rtCfg: rtCfg,
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

// getUpstream 获取当前上游 URL 和 API Key（优先 RuntimeConfig）
func (c *Client) getUpstream() (baseURL, apiKey string) {
	if c.rtCfg != nil {
		url, key := c.rtCfg.GetUpstream()
		if url != "" {
			return url, key
		}
	}
	return c.baseURL, c.apiKey
}

// HasAPIKey 检查上游是否配置了 API Key
func (c *Client) HasAPIKey() bool {
	_, key := c.getUpstream()
	return key != ""
}

func (c *Client) DoJSON(ctx context.Context, path string, payload any, headers map[string]string) (*http.Response, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}

	baseURL, apiKey := c.getUpstream()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.applyAuth(req, apiKey, headers)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, err
	}
	return resp, data, nil
}

func (c *Client) DoStream(ctx context.Context, path string, payload any, headers map[string]string) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	baseURL, apiKey := c.getUpstream()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	c.applyAuth(req, apiKey, headers)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		log.Printf("ERROR: upstream stream HTTP %d: %s", resp.StatusCode, truncateStr(string(data), 500))
		return resp, fmt.Errorf("upstream error: %s", strings.TrimSpace(string(data)))
	}
	return resp, nil
}

// ListModels 获取上游模型列表（GET /v1/models）
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	baseURL, apiKey := c.getUpstream()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upstream models API error %d: %s", resp.StatusCode, truncateStr(string(data), 500))
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode models response: %w", err)
	}

	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

func (c *Client) applyAuth(req *http.Request, apiKey string, headers map[string]string) {
	if headers != nil {
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
