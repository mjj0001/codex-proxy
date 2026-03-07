/**
 * Codex 执行器模块
 * 负责向 Codex API 发送请求并处理响应
 * 支持流式和非流式两种模式，处理认证头注入、错误处理和重试
 */
package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"codex-proxy/internal/auth"
	"codex-proxy/internal/thinking"
	"codex-proxy/internal/translator"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

/* Codex 客户端版本常量，用于请求头 */
const (
	codexClientVersion = "0.101.0"
	codexUserAgent     = "codex_cli_rs/0.101.0 (Mac OS 26.0.1; arm64) Apple_Terminal/464"
)

/**
 * Executor Codex 请求执行器
 * 使用全局共享连接池提升高并发性能
 * @field baseURL - Codex API 基础 URL
 * @field httpClient - 共享的 HTTP 客户端（连接池复用）
 */
type Executor struct {
	baseURL    string
	httpClient *http.Client
}

/**
 * NewExecutor 创建新的 Codex 执行器
 * 初始化全局连接池，支持高并发场景
 * @param baseURL - API 基础 URL
 * @param proxyURL - 代理地址
 * @returns *Executor - 执行器实例
 */
func NewExecutor(baseURL, proxyURL string) *Executor {
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: false},
	}

	if proxyURL != "" {
		proxyParsed, err := url.Parse(proxyURL)
		if err == nil {
			transport.Proxy = http.ProxyURL(proxyParsed)
		}
	}

	return &Executor{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Minute,
		},
	}
}

/**
 * ExecuteStream 执行流式请求
 * 将 OpenAI 格式请求转换为 Codex 格式，发送并以 SSE 方式返回响应
 *
 * @param ctx - 上下文
 * @param account - 使用的账号
 * @param requestBody - 原始 OpenAI Chat Completions 请求体
 * @param model - 模型名称（可能含思考后缀）
 * @param writer - HTTP 响应写入器
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteStream(ctx context.Context, account *auth.Account, requestBody []byte, model string, writer http.ResponseWriter) error {
	/* 应用思考配置并获取真实模型名 */
	body, baseModel := thinking.ApplyThinking(requestBody, model)

	/* 转换请求格式：OpenAI → Codex */
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)

	/* 设置模型和流式参数 */
	codexBody, _ = sjson.SetBytes(codexBody, "model", baseModel)
	codexBody, _ = sjson.DeleteBytes(codexBody, "previous_response_id")
	codexBody, _ = sjson.DeleteBytes(codexBody, "prompt_cache_retention")
	codexBody, _ = sjson.DeleteBytes(codexBody, "safety_identifier")
	/* #1901: 剔除 generate 参数，非 Codex 原生后端不支持此参数 */
	codexBody, _ = sjson.DeleteBytes(codexBody, "generate")
	if !gjson.GetBytes(codexBody, "instructions").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "instructions", "")
	}

	/* 构建 HTTP 请求 */
	apiURL := e.baseURL + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(codexBody))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}

	applyCodexHeaders(httpReq, account, true)

	/* 发送请求（使用全局连接池） */
	httpResp, err := e.httpClient.Do(httpReq)
	if err != nil {
		account.RecordFailure()
		return fmt.Errorf("请求发送失败: %w", err)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	/* 处理错误状态码 */
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(httpResp.Body)
		log.Errorf("Codex API 错误 [%d]: %s", httpResp.StatusCode, summarizeError(errBody))

		/* 根据状态码反馈账号状态 */
		handleAccountError(account, httpResp.StatusCode, errBody)

		return &StatusError{Code: httpResp.StatusCode, Body: errBody}
	}

	/* 设置 SSE 响应头 */
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)

	flusher, canFlush := writer.(http.Flusher)

	/* 构建反向工具名映射 */
	reverseToolMap := translator.BuildReverseToolNameMap(requestBody)
	state := translator.NewStreamState(baseModel)

	/* 逐行读取 SSE 事件并转换 */
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800)

	for scanner.Scan() {
		line := scanner.Bytes()
		/* 提取流式 usage */
		extractUsageFromStreamLine(line, account)
		chunks := translator.ConvertStreamChunk(ctx, line, state, reverseToolMap)
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(writer, "data: %s\n\n", chunk)
			if canFlush {
				flusher.Flush()
			}
		}
	}

	if err = scanner.Err(); err != nil {
		log.Errorf("读取流式响应失败: %v", err)
		return err
	}

	/* 发送结束标记 */
	_, _ = fmt.Fprintf(writer, "data: [DONE]\n\n")
	if canFlush {
		flusher.Flush()
	}

	return nil
}

/**
 * ExecuteNonStream 执行非流式请求
 * 将 OpenAI 格式请求转换为 Codex 格式，发送并返回完整响应
 *
 * @param ctx - 上下文
 * @param account - 使用的账号
 * @param requestBody - 原始 OpenAI Chat Completions 请求体
 * @param model - 模型名称（可能含思考后缀）
 * @returns []byte - OpenAI Chat Completions 格式的响应 JSON
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteNonStream(ctx context.Context, account *auth.Account, requestBody []byte, model string) ([]byte, error) {
	/* 应用思考配置 */
	body, baseModel := thinking.ApplyThinking(requestBody, model)

	/* 转换请求格式 */
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	codexBody, _ = sjson.SetBytes(codexBody, "model", baseModel)
	codexBody, _ = sjson.SetBytes(codexBody, "stream", true)
	codexBody, _ = sjson.DeleteBytes(codexBody, "previous_response_id")
	codexBody, _ = sjson.DeleteBytes(codexBody, "prompt_cache_retention")
	codexBody, _ = sjson.DeleteBytes(codexBody, "safety_identifier")
	/* #1901: 剔除 generate 参数 */
	codexBody, _ = sjson.DeleteBytes(codexBody, "generate")
	if !gjson.GetBytes(codexBody, "instructions").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "instructions", "")
	}

	/* 构建并发送请求 */
	apiURL := e.baseURL + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(codexBody))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	applyCodexHeaders(httpReq, account, true)

	httpResp, err := e.httpClient.Do(httpReq)
	if err != nil {
		account.RecordFailure()
		return nil, fmt.Errorf("请求发送失败: %w", err)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(httpResp.Body)
		handleAccountError(account, httpResp.StatusCode, errBody)
		return nil, &StatusError{Code: httpResp.StatusCode, Body: errBody}
	}

	/* 读取完整响应 */
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	/* 提取 usage 统计 */
	extractUsageFromSSE(data, account)

	/* 从 SSE 数据中找到 response.completed 事件并转换 */
	reverseToolMap := translator.BuildReverseToolNameMap(requestBody)
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		jsonData := bytes.TrimSpace(line[5:])
		if gjson.GetBytes(jsonData, "type").String() != "response.completed" {
			continue
		}
		result := translator.ConvertNonStreamResponse(ctx, jsonData, reverseToolMap)
		if result != "" {
			return []byte(result), nil
		}
	}

	return nil, fmt.Errorf("未收到 response.completed 事件")
}

/**
 * ExecuteResponsesStream 执行 Responses API 流式请求
 * 直接透传 Codex SSE 事件到客户端，不做 Chat Completions 格式转换
 *
 * @param ctx - 上下文
 * @param account - 使用的账号
 * @param requestBody - Responses API 格式的请求体
 * @param model - 模型名称（可能含思考后缀）
 * @param writer - HTTP 响应写入器
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteResponsesStream(ctx context.Context, account *auth.Account, requestBody []byte, model string, writer http.ResponseWriter) error {
	/* 应用思考配置并获取真实模型名 */
	body, baseModel := thinking.ApplyThinking(requestBody, model)

	/* Responses API 格式已经很接近 Codex 格式，使用通用转换器处理 */
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	codexBody, _ = sjson.SetBytes(codexBody, "model", baseModel)
	codexBody, _ = sjson.DeleteBytes(codexBody, "previous_response_id")
	codexBody, _ = sjson.DeleteBytes(codexBody, "prompt_cache_retention")
	codexBody, _ = sjson.DeleteBytes(codexBody, "safety_identifier")
	codexBody, _ = sjson.DeleteBytes(codexBody, "generate")
	if !gjson.GetBytes(codexBody, "instructions").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "instructions", "")
	}

	/* 构建 HTTP 请求 */
	apiURL := e.baseURL + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(codexBody))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}

	applyCodexHeaders(httpReq, account, true)

	/* 发送请求 */
	httpResp, err := e.httpClient.Do(httpReq)
	if err != nil {
		account.RecordFailure()
		return fmt.Errorf("请求发送失败: %w", err)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	/* 处理错误状态码 */
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(httpResp.Body)
		log.Errorf("Codex API 错误 [%d]: %s", httpResp.StatusCode, summarizeError(errBody))
		handleAccountError(account, httpResp.StatusCode, errBody)
		return &StatusError{Code: httpResp.StatusCode, Body: errBody}
	}

	/* 设置 SSE 响应头 */
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)

	flusher, canFlush := writer.(http.Flusher)

	/* 直接透传 SSE 事件，不做格式转换 */
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800)

	for scanner.Scan() {
		line := scanner.Bytes()
		/* 提取流式 usage */
		extractUsageFromStreamLine(line, account)
		if len(line) == 0 {
			_, _ = fmt.Fprint(writer, "\n")
		} else {
			_, _ = fmt.Fprintf(writer, "%s\n", line)
		}
		if canFlush {
			flusher.Flush()
		}
	}

	if err = scanner.Err(); err != nil {
		log.Errorf("读取流式响应失败: %v", err)
		return err
	}

	return nil
}

/**
 * ExecuteResponsesNonStream 执行 Responses API 非流式请求
 * 从 Codex SSE 响应中提取 response.completed 事件，返回原生 response 对象
 *
 * @param ctx - 上下文
 * @param account - 使用的账号
 * @param requestBody - Responses API 格式的请求体
 * @param model - 模型名称（可能含思考后缀）
 * @returns []byte - Codex Responses API 格式的完整响应 JSON
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteResponsesNonStream(ctx context.Context, account *auth.Account, requestBody []byte, model string) ([]byte, error) {
	/* 应用思考配置 */
	body, baseModel := thinking.ApplyThinking(requestBody, model)

	/* 转换请求格式 */
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	codexBody, _ = sjson.SetBytes(codexBody, "model", baseModel)
	codexBody, _ = sjson.SetBytes(codexBody, "stream", true)
	codexBody, _ = sjson.DeleteBytes(codexBody, "previous_response_id")
	codexBody, _ = sjson.DeleteBytes(codexBody, "prompt_cache_retention")
	codexBody, _ = sjson.DeleteBytes(codexBody, "safety_identifier")
	codexBody, _ = sjson.DeleteBytes(codexBody, "generate")
	if !gjson.GetBytes(codexBody, "instructions").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "instructions", "")
	}

	/* 构建并发送请求 */
	apiURL := e.baseURL + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(codexBody))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	applyCodexHeaders(httpReq, account, true)

	httpResp, err := e.httpClient.Do(httpReq)
	if err != nil {
		account.RecordFailure()
		return nil, fmt.Errorf("请求发送失败: %w", err)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(httpResp.Body)
		handleAccountError(account, httpResp.StatusCode, errBody)
		return nil, &StatusError{Code: httpResp.StatusCode, Body: errBody}
	}

	/* 读取完整响应 */
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	/* 提取 usage 统计 */
	extractUsageFromSSE(data, account)

	/* 从 SSE 数据中找到 response.completed 事件，提取 response 对象 */
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		jsonData := bytes.TrimSpace(line[5:])
		if gjson.GetBytes(jsonData, "type").String() != "response.completed" {
			continue
		}
		/* 返回 response 对象（Responses API 原生格式） */
		if resp := gjson.GetBytes(jsonData, "response"); resp.Exists() {
			return []byte(resp.Raw), nil
		}
	}

	return nil, fmt.Errorf("未收到 response.completed 事件")
}

/**
 * applyCodexHeaders 设置 Codex API 请求头
 * @param r - HTTP 请求
 * @param account - 账号（提供 access_token 和 account_id）
 * @param stream - 是否为流式请求
 */
func applyCodexHeaders(r *http.Request, account *auth.Account, stream bool) {
	token := account.GetAccessToken()
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Version", codexClientVersion)
	r.Header.Set("Session_id", uuid.NewString())
	r.Header.Set("User-Agent", codexUserAgent)
	r.Header.Set("Originator", "codex_cli_rs")
	r.Header.Set("Connection", "Keep-Alive")

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}

	accountID := account.GetAccountID()
	if accountID != "" {
		r.Header.Set("Chatgpt-Account-Id", accountID)
	}
}

/**
 * handleAccountError 根据 HTTP 错误状态码记录账号失败
 * handler 层会根据 ShouldRemoveAccount 决定是否删号
 * @param account - 账号
 * @param statusCode - HTTP 状态码
 * @param body - 错误响应体
 */
func handleAccountError(account *auth.Account, statusCode int, body []byte) {
	account.RecordFailure()

	switch {
	case statusCode == 429:
		/* 配额耗尽，设置配额冷却 */
		cooldown := parseRetryAfter(body)
		if cooldown > 0 {
			account.SetQuotaCooldown(cooldown)
		}
	case statusCode == 403:
		account.SetCooldown(5 * time.Minute)
	}

	log.Warnf("账号 [%s] 请求失败 [%d]: %s", account.GetEmail(), statusCode, summarizeError(body))
}

/**
 * ShouldRemoveAccount 判断此错误是否应该导致账号被删除（内存+磁盘）
 * 仅 401（认证失效）认为是账号永久失效，需要删除
 * 403（地域封锁/Cloudflare 拦截）、400（请求参数错误）、429（限频）、5xx（服务端问题）均不删号
 * @param statusCode - HTTP 状态码
 * @returns bool - 是否应该删除
 */
func ShouldRemoveAccount(statusCode int) bool {
	/* 仅 401 认证失效才删除账号 */
	return statusCode == 401
}

/**
 * StatusError HTTP 状态错误
 * @field Code - HTTP 状态码
 * @field Body - 错误响应体
 */
type StatusError struct {
	Code int
	Body []byte
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("Codex API 错误 [%d]: %s", e.Code, summarizeError(e.Body))
}

/**
 * summarizeError 提取错误响应的摘要信息
 * @param body - 错误响应体
 * @returns string - 错误摘要
 */
func summarizeError(body []byte) string {
	if len(body) == 0 {
		return "(空响应)"
	}
	if msg := gjson.GetBytes(body, "error.message").String(); msg != "" {
		return msg
	}
	if len(body) > 200 {
		return string(body[:200]) + "..."
	}
	return string(body)
}

/**
 * parseRetryAfter 从 429 错误响应中解析冷却时间
 * @param body - 错误响应体
 * @returns time.Duration - 冷却持续时间
 */
func parseRetryAfter(body []byte) time.Duration {
	if len(body) == 0 {
		return 60 * time.Second
	}

	/* 尝试从 resets_at 字段解析 */
	if resetsAt := gjson.GetBytes(body, "error.resets_at").Int(); resetsAt > 0 {
		resetTime := time.Unix(resetsAt, 0)
		if resetTime.After(time.Now()) {
			return time.Until(resetTime)
		}
	}

	/* 尝试从 resets_in_seconds 字段解析 */
	if seconds := gjson.GetBytes(body, "error.resets_in_seconds").Int(); seconds > 0 {
		return time.Duration(seconds) * time.Second
	}

	/* 默认冷却 60 秒 */
	return 60 * time.Second
}

/**
 * extractUsageFromSSE 从 SSE 数据中提取 response.completed 事件的 usage 信息
 * 并记录到账号的 token 使用量统计
 * @param data - 完整的 SSE 响应数据
 * @param account - 要记录 usage 的账号
 */
func extractUsageFromSSE(data []byte, account *auth.Account) {
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		jsonData := bytes.TrimSpace(line[5:])
		if gjson.GetBytes(jsonData, "type").String() != "response.completed" {
			continue
		}
		usage := gjson.GetBytes(jsonData, "response.usage")
		if !usage.Exists() {
			continue
		}
		inputTokens := usage.Get("input_tokens").Int()
		outputTokens := usage.Get("output_tokens").Int()
		totalTokens := usage.Get("total_tokens").Int()
		account.RecordUsage(inputTokens, outputTokens, totalTokens)
		return
	}
}

/**
 * extractUsageFromStreamLine 从单行 SSE 数据中提取 usage（用于流式场景）
 * @param line - 单行 SSE 数据
 * @param account - 要记录 usage 的账号
 */
func extractUsageFromStreamLine(line []byte, account *auth.Account) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	jsonData := bytes.TrimSpace(line[5:])
	if gjson.GetBytes(jsonData, "type").String() != "response.completed" {
		return
	}
	usage := gjson.GetBytes(jsonData, "response.usage")
	if !usage.Exists() {
		return
	}
	inputTokens := usage.Get("input_tokens").Int()
	outputTokens := usage.Get("output_tokens").Int()
	totalTokens := usage.Get("total_tokens").Int()
	account.RecordUsage(inputTokens, outputTokens, totalTokens)
}
