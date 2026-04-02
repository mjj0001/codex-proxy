package handler

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	fasthttprouter "github.com/fasthttp/router"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

type openAIErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func TestResponsesCompactRoute(t *testing.T) {
	tests := []struct {
		name        string
		apiKeys     []string
		headers     map[string]string
		body        []byte
		wantStatus  int
		wantMessage string
		wantType    string
		wantCode    string
	}{
		{
			name:        "未配置鉴权时空请求命中 compact handler",
			wantStatus:  fasthttp.StatusBadRequest,
			wantMessage: "读取请求体失败",
			wantType:    "invalid_request_error",
		},
		{
			name:        "未配置鉴权时缺少 model 返回 400",
			body:        []byte(`{}`),
			wantStatus:  fasthttp.StatusBadRequest,
			wantMessage: "缺少 model 字段",
			wantType:    "invalid_request_error",
		},
		{
			name:        "配置鉴权后未带 key 返回 401",
			apiKeys:     []string{"test-key"},
			body:        []byte(`{}`),
			wantStatus:  fasthttp.StatusUnauthorized,
			wantMessage: "无效的 API Key",
			wantType:    "invalid_request_error",
			wantCode:    "invalid_api_key",
		},
		{
			name:    "配置鉴权后带合法 key 命中 compact handler",
			apiKeys: []string{"test-key"},
			headers: map[string]string{
				"Authorization": "Bearer test-key",
			},
			body:        []byte(`{}`),
			wantStatus:  fasthttp.StatusBadRequest,
			wantMessage: "缺少 model 字段",
			wantType:    "invalid_request_error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, respBody := performCompactRequest(t, tc.apiKeys, tc.headers, tc.body)
			if status != tc.wantStatus {
				t.Fatalf("状态码不符合预期: got=%d want=%d body=%s", status, tc.wantStatus, string(respBody))
			}

			var resp openAIErrorEnvelope
			if err := json.Unmarshal(respBody, &resp); err != nil {
				t.Fatalf("解析响应失败: %v body=%s", err, string(respBody))
			}
			if resp.Error.Message != tc.wantMessage {
				t.Fatalf("错误消息不符合预期: got=%q want=%q", resp.Error.Message, tc.wantMessage)
			}
			if resp.Error.Type != tc.wantType {
				t.Fatalf("错误类型不符合预期: got=%q want=%q", resp.Error.Type, tc.wantType)
			}
			if resp.Error.Code != tc.wantCode {
				t.Fatalf("错误代码不符合预期: got=%q want=%q", resp.Error.Code, tc.wantCode)
			}
		})
	}
}

func performCompactRequest(t *testing.T, apiKeys []string, headers map[string]string, body []byte) (int, []byte) {
	t.Helper()

	r := fasthttprouter.New()
	(&ProxyHandler{apiKeys: apiKeys}).RegisterRoutes(r)

	ln := fasthttputil.NewInmemoryListener()
	srv := &fasthttp.Server{
		Handler:          r.Handler,
		DisableKeepalive: true,
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- srv.Serve(ln)
	}()
	defer func() {
		_ = srv.Shutdown()
		_ = ln.Close()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Fatal("测试服务器关闭超时")
		}
	}()

	client := &fasthttp.Client{
		Dial: func(_ string) (net.Conn, error) {
			return ln.Dial()
		},
	}

	var req fasthttp.Request
	var resp fasthttp.Response
	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetRequestURI("http://compact.test/v1/responses/compact")
	req.Header.SetConnectionClose()
	if body != nil {
		req.Header.SetContentType("application/json")
		req.SetBody(body)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	if err := client.Do(&req, &resp); err != nil {
		t.Fatalf("发送请求失败: %v", err)
	}

	respBody := append([]byte(nil), resp.Body()...)
	return resp.StatusCode(), respBody
}
