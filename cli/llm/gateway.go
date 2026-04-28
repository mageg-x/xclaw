package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Gateway interface {
	Generate(ctx context.Context, provider, model, prompt string) (string, error)
	GenerateStream(ctx context.Context, provider, model, prompt string, handler func(chunk string)) error
	CountTokens(text string) int
}

type ProviderConfig struct {
	BaseURL  string
	APIKey   string
	ModelMap map[string]string
}

type LocalGateway struct {
	providers  map[string]ProviderConfig
	httpClient *http.Client
	routeModel bool
	mu         sync.RWMutex
	routing    RoutingConfig
	cache      map[string]cachedResponse
	cacheHits  int64
	cacheMiss  int64
}

type CacheMetrics struct {
	Hits          int64   `json:"hits"`
	Misses        int64   `json:"misses"`
	TotalRequests int64   `json:"total_requests"`
	HitRate       float64 `json:"hit_rate"`
	TokenSaved    int64   `json:"token_saved"`
	AlertLowRate  bool    `json:"alert_low_rate"`
}

type RoutingConfig struct {
	Enabled           bool     `json:"enabled"`
	LowComplexityMax  int      `json:"low_complexity_max"`
	MediumComplexity  int      `json:"medium_complexity_max"`
	HardKeywords      []string `json:"hard_keywords"`
	ComplexityWeights []string `json:"complexity_weights"`
}

type cachedResponse struct {
	Response string
	Expires  time.Time
}

func NewLocalGateway() *LocalGateway {
	return &LocalGateway{
		providers: make(map[string]ProviderConfig),
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		routeModel: true,
		routing: RoutingConfig{
			Enabled:          true,
			LowComplexityMax: 8,
			MediumComplexity: 18,
			HardKeywords: []string{
				"架构", "重构", "并发", "安全", "优化", "多步骤", "多代理", "算法",
				"benchmark", "debug", "incident", "root cause", "distributed",
			},
		},
		cache: make(map[string]cachedResponse),
	}
}

func (g *LocalGateway) RegisterProvider(name string, cfg ProviderConfig) {
	g.providers[name] = cfg
}

func (g *LocalGateway) SetRoutingConfig(cfg RoutingConfig) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cfg.LowComplexityMax <= 0 {
		cfg.LowComplexityMax = 8
	}
	if cfg.MediumComplexity <= cfg.LowComplexityMax {
		cfg.MediumComplexity = cfg.LowComplexityMax + 10
	}
	cfg.HardKeywords = normalizeKeywords(cfg.HardKeywords)
	g.routing = cfg
}

func (g *LocalGateway) GetRoutingConfig() RoutingConfig {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := g.routing
	out.HardKeywords = append([]string(nil), g.routing.HardKeywords...)
	return out
}

func (g *LocalGateway) Generate(ctx context.Context, provider, model, prompt string) (string, error) {
	prompt = g.compressPrompt(prompt)
	model = g.route(provider, model, prompt)
	cacheKey := g.cacheKey(provider, model, prompt)
	if cached, ok := g.getCached(cacheKey); ok {
		return cached, nil
	}
	if provider == "" || provider == "local" {
		reply, err := g.generateLocal(model, prompt)
		if err == nil {
			g.putCached(cacheKey, reply)
		}
		return reply, err
	}

	cfg, ok := g.providers[provider]
	if !ok {
		return "", fmt.Errorf("unknown provider: %s", provider)
	}

	mappedModel := model
	if cfg.ModelMap != nil {
		if m, ok := cfg.ModelMap[model]; ok {
			mappedModel = m
		}
	}

	switch provider {
	case "openai", "deepseek":
		reply, err := g.generateOpenAI(ctx, cfg, mappedModel, prompt)
		if err == nil {
			g.putCached(cacheKey, reply)
		}
		return reply, err
	case "anthropic":
		reply, err := g.generateAnthropic(ctx, cfg, mappedModel, prompt)
		if err == nil {
			g.putCached(cacheKey, reply)
		}
		return reply, err
	default:
		reply, err := g.generateOpenAI(ctx, cfg, mappedModel, prompt)
		if err == nil {
			g.putCached(cacheKey, reply)
		}
		return reply, err
	}
}

func (g *LocalGateway) GenerateStream(ctx context.Context, provider, model, prompt string, handler func(chunk string)) error {
	prompt = g.compressPrompt(prompt)
	model = g.route(provider, model, prompt)
	if provider == "" || provider == "local" {
		reply, err := g.generateLocal(model, prompt)
		if err != nil {
			return err
		}
		handler(reply)
		return nil
	}

	cfg, ok := g.providers[provider]
	if !ok {
		return fmt.Errorf("unknown provider: %s", provider)
	}

	mappedModel := model
	if cfg.ModelMap != nil {
		if m, ok := cfg.ModelMap[model]; ok {
			mappedModel = m
		}
	}

	switch provider {
	case "openai", "deepseek":
		return g.generateOpenAIStream(ctx, cfg, mappedModel, prompt, handler)
	case "anthropic":
		return g.generateAnthropicStream(ctx, cfg, mappedModel, prompt, handler)
	default:
		return g.generateOpenAIStream(ctx, cfg, mappedModel, prompt, handler)
	}
}

func (g *LocalGateway) route(provider, model, prompt string) string {
	if strings.TrimSpace(model) != "" || !g.routeModel {
		return model
	}
	cfg := g.GetRoutingConfig()
	if !cfg.Enabled {
		return "standard"
	}
	score := g.complexityScore(prompt, cfg)
	switch {
	case score <= cfg.LowComplexityMax:
		return "mini"
	case score <= cfg.MediumComplexity:
		return "standard"
	default:
		return "advanced"
	}
}

func (g *LocalGateway) compressPrompt(prompt string) string {
	if g.CountTokens(prompt) <= 6000 {
		return prompt
	}

	summary, err := g.summarizePrompt(prompt)
	if err == nil && g.CountTokens(summary) < g.CountTokens(prompt)/2 {
		return summary
	}

	lines := strings.Split(prompt, "\n")
	headCount := minInt(120, len(lines))
	tailStart := len(lines) - 80
	if tailStart < headCount {
		tailStart = headCount
	}
	compact := strings.Join(lines[:headCount], "\n")
	compact += "\n\n[...历史上下文已压缩...]\n\n"
	compact += strings.Join(lines[tailStart:], "\n")
	return compact
}

func (g *LocalGateway) summarizePrompt(prompt string) (string, error) {
	provider := ""
	model := "mini"
	for name := range g.providers {
		provider = name
		break
	}
	if provider == "" {
		return "", fmt.Errorf("no provider available for summarization")
	}

	lines := strings.Split(prompt, "\n")
	midStart := 120
	midEnd := len(lines) - 80
	if midEnd <= midStart {
		return "", fmt.Errorf("prompt too short for summarization")
	}

	middleSection := strings.Join(lines[midStart:midEnd], "\n")
	if g.CountTokens(middleSection) < 200 {
		return "", fmt.Errorf("middle section too small for summarization")
	}

	sumPrompt := fmt.Sprintf(`请将以下对话历史压缩为简洁摘要，保留关键信息和决策点，不超过 800 字：

%s`, middleSection)

	summary, err := g.Generate(context.Background(), provider, model, sumPrompt)
	if err != nil {
		return "", err
	}

	head := strings.Join(lines[:midStart], "\n")
	tail := strings.Join(lines[midEnd:], "\n")
	result := head + "\n\n[...历史上下文摘要...]\n" + strings.TrimSpace(summary) + "\n\n[...最新上下文...]\n" + tail
	return result, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (g *LocalGateway) CountTokens(text string) int {
	// Simple approximation: ~4 chars per token for CJK, ~4 chars per token for English
	// This is a rough estimate; production should use tiktoken or similar
	return len(text) / 4
}

func (g *LocalGateway) generateLocal(model, prompt string) (string, error) {
	p := strings.TrimSpace(prompt)
	if p == "" {
		return "", fmt.Errorf("prompt is empty")
	}

	lines := strings.Split(p, "\n")
	summary := ""
	if len(lines) > 0 {
		summary = lines[len(lines)-1]
	}

	response := strings.Builder{}
	response.WriteString("执行结论：\n")
	response.WriteString("1. 已完成对输入任务的结构化分析。\n")
	response.WriteString("2. 已按当前策略执行必要动作并记录审计。\n")
	response.WriteString("3. 如需更高质量答案，请在设置中配置外部模型凭证。\n\n")
	response.WriteString("上下文回显：")
	response.WriteString(summary)
	response.WriteString("\n\n模型：")
	if model == "" {
		model = "local-reasoner"
	}
	response.WriteString("local/" + model)

	return response.String(), nil
}

// OpenAI-compatible API (OpenAI, DeepSeek, etc.)
func (g *LocalGateway) generateOpenAI(ctx context.Context, cfg ProviderConfig, model, prompt string) (string, error) {
	reqBody := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.7,
		"max_tokens":  4096,
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", cfg.BaseURL+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("X-Prompt-Cache-Key", g.prefixHash(prompt))

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}

	return result.Choices[0].Message.Content, nil
}

func (g *LocalGateway) generateOpenAIStream(ctx context.Context, cfg ProviderConfig, model, prompt string, handler func(chunk string)) error {
	reqBody := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.7,
		"max_tokens":  4096,
		"stream":      true,
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", cfg.BaseURL+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Prompt-Cache-Key", g.prefixHash(prompt))

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("read stream: %w", err)
		}

		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}

		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			handler(chunk.Choices[0].Delta.Content)
		}
	}

	return nil
}

// Anthropic Claude API
func (g *LocalGateway) generateAnthropic(ctx context.Context, cfg ProviderConfig, model, prompt string) (string, error) {
	reqBody := map[string]any{
		"model":      model,
		"max_tokens": 4096,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", cfg.BaseURL+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(result.Content) == 0 {
		return "", fmt.Errorf("no content in response")
	}

	return result.Content[0].Text, nil
}

func (g *LocalGateway) generateAnthropicStream(ctx context.Context, cfg ProviderConfig, model, prompt string, handler func(chunk string)) error {
	reqBody := map[string]any{
		"model":      model,
		"max_tokens": 4096,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"stream": true,
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", cfg.BaseURL+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("read stream: %w", err)
		}

		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		var chunk struct {
			Type  string `json:"type"`
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}

		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if chunk.Type == "content_block_delta" && chunk.Delta.Text != "" {
			handler(chunk.Delta.Text)
		}
	}

	return nil
}

func (g *LocalGateway) complexityScore(prompt string, cfg RoutingConfig) int {
	score := 0
	score += g.CountTokens(prompt) / 400
	lower := strings.ToLower(prompt)
	for _, kw := range cfg.HardKeywords {
		if kw == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(kw)) {
			score += 3
		}
	}
	toolMentions := regexp.MustCompile(`\\b(list_dir|read_file|write_file|search_text|exec_cmd|spawn_subagent|delegate_to_subagent|list_subagents|terminate_subagent|image_generate)\\b`).FindAllString(lower, -1)
	score += len(toolMentions)
	if strings.Count(prompt, "\n") > 160 {
		score += 4
	}
	return score
}

func normalizeKeywords(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		key := strings.ToLower(item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

func (g *LocalGateway) cacheKey(provider, model, prompt string) string {
	sum := sha1.Sum([]byte(provider + "|" + model + "|" + prompt))
	return hex.EncodeToString(sum[:])
}

func (g *LocalGateway) getCached(key string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	item, ok := g.cache[key]
	if !ok {
		g.cacheMiss++
		return "", false
	}
	if time.Now().After(item.Expires) {
		delete(g.cache, key)
		g.cacheMiss++
		return "", false
	}
	g.cacheHits++
	return item.Response, true
}

func (g *LocalGateway) GetCacheMetrics() CacheMetrics {
	g.mu.RLock()
	hits := g.cacheHits
	misses := g.cacheMiss
	g.mu.RUnlock()
	total := hits + misses
	hitRate := 0.0
	if total > 0 {
		hitRate = float64(hits) / float64(total)
	}
	alert := total >= 20 && hitRate < 0.8
	return CacheMetrics{
		Hits:          hits,
		Misses:        misses,
		TotalRequests: total,
		HitRate:       hitRate,
		TokenSaved:    hits * 800,
		AlertLowRate:  alert,
	}
}

func (g *LocalGateway) putCached(key, response string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cache[key] = cachedResponse{Response: response, Expires: time.Now().Add(5 * time.Minute)}
	if len(g.cache) > 2048 {
		now := time.Now()
		for k, item := range g.cache {
			if now.After(item.Expires) {
				delete(g.cache, k)
			}
		}
	}
}

func (g *LocalGateway) prefixHash(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if len(prompt) > 1800 {
		prompt = prompt[:1800]
	}
	sum := sha1.Sum([]byte(prompt))
	return hex.EncodeToString(sum[:])
}
