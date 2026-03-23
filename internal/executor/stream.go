package executor

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"codex-proxy/internal/auth"
	"codex-proxy/internal/thinking"
	"codex-proxy/internal/translator"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

// CodexResponsesStream 上游 /responses 已返回 2xx 后的可读流（由 Pump* 负责关闭 Body）。
type CodexResponsesStream struct {
	body         io.ReadCloser
	account      *auth.Account
	Attempts     int
	BaseModel    string
	ConvertDur   time.Duration
	SendDur      time.Duration
	reverseTools map[string]string
}

// OpenCodexResponsesStream 完成选号、重试与首包前的 HTTP 往返；调用方在写入客户端 SSE 头后再 Pump。
func (e *Executor) OpenCodexResponsesStream(ctx context.Context, rc RetryConfig, requestBody []byte, model string) (*CodexResponsesStream, error) {
	convertStart := time.Now()
	body, baseModel := thinking.ApplyThinking(requestBody, model)
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	convertDur := time.Since(convertStart)
	apiURL := e.baseURL + "/responses"
	sendStart := time.Now()
	httpResp, account, attempts, err := e.sendWithRetry(ctx, rc, model, apiURL, codexBody, true)
	if err != nil {
		return nil, err
	}
	return &CodexResponsesStream{
		body:         httpResp.Body,
		account:      account,
		Attempts:     attempts,
		BaseModel:    baseModel,
		ConvertDur:   convertDur,
		SendDur:      time.Since(sendStart),
		reverseTools: translator.BuildReverseToolNameMap(requestBody),
	}, nil
}

// PumpChatCompletion 将 Codex SSE 转为 OpenAI Chat Completions 块写入 w（客户端 SSE 头须已由 handler 写好）。
func (s *CodexResponsesStream) PumpChatCompletion(w io.Writer, flush func()) error {
	defer func() { _ = s.body.Close() }()

	reverseToolMap := s.reverseTools
	state := translator.NewStreamState(s.BaseModel)
	scanner := bufio.NewScanner(s.body)
	scanner.Buffer(make([]byte, scannerInitSize), scannerMaxSize)
	streamStart := time.Now()
	var firstChunkAt time.Time
	var completedAt time.Time
	chunkCount := 0
	pumpCtx := context.Background()

	for scanner.Scan() {
		line := scanner.Bytes()
		chunks := translator.ConvertStreamChunk(pumpCtx, line, state, reverseToolMap)
		for _, chunk := range chunks {
			if firstChunkAt.IsZero() {
				firstChunkAt = time.Now()
			}
			chunkCount++
			_, _ = w.Write(sseDataPrefix)
			_, _ = io.WriteString(w, chunk)
			_, _ = w.Write(sseDataSuffix)
			if flush != nil {
				flush()
			}
		}
		if state.Completed {
			if completedAt.IsZero() {
				completedAt = time.Now()
			}
			break
		}
	}

	if err := scanner.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			firstChunkDur := time.Duration(0)
			if !firstChunkAt.IsZero() {
				firstChunkDur = firstChunkAt.Sub(streamStart)
			}
			log.Infof("req summary stream model=%s account=%s attempts=%d convert=%v upstream_ttfb=%v first_chunk=%v to_completed=%v tail_after_completed=%v stream=%v chunks=%d total=%v (canceled)", s.BaseModel, s.account.GetEmail(), s.Attempts, s.ConvertDur, s.SendDur, firstChunkDur, time.Duration(0), time.Duration(0), time.Since(streamStart), chunkCount, time.Since(streamStart))
			return nil
		}
		log.Errorf("读取流式响应失败: %v", err)
		firstChunkDur := time.Duration(0)
		completedDur := time.Duration(0)
		tailAfterCompleted := time.Duration(0)
		if !firstChunkAt.IsZero() {
			firstChunkDur = firstChunkAt.Sub(streamStart)
		}
		if !completedAt.IsZero() {
			completedDur = completedAt.Sub(streamStart)
			tailAfterCompleted = time.Since(completedAt)
		}
		log.Infof("req summary stream model=%s account=%s attempts=%d convert=%v upstream_ttfb=%v first_chunk=%v to_completed=%v tail_after_completed=%v stream=%v chunks=%d total=%v (ERR)", s.BaseModel, s.account.GetEmail(), s.Attempts, s.ConvertDur, s.SendDur, firstChunkDur, completedDur, tailAfterCompleted, time.Since(streamStart), chunkCount, time.Since(streamStart))
		return wrapReadErr(err)
	}
	if !state.HasText && !state.HasToolCall && !state.HasReasoning {
		firstChunkDur := time.Duration(0)
		completedDur := time.Duration(0)
		tailAfterCompleted := time.Duration(0)
		if !firstChunkAt.IsZero() {
			firstChunkDur = firstChunkAt.Sub(streamStart)
		}
		if !completedAt.IsZero() {
			completedDur = completedAt.Sub(streamStart)
			tailAfterCompleted = time.Since(completedAt)
		}
		log.Infof("req summary stream model=%s account=%s attempts=%d convert=%v upstream_ttfb=%v first_chunk=%v to_completed=%v tail_after_completed=%v stream=%v chunks=%d total=%v (empty)", s.BaseModel, s.account.GetEmail(), s.Attempts, s.ConvertDur, s.SendDur, firstChunkDur, completedDur, tailAfterCompleted, time.Since(streamStart), chunkCount, time.Since(streamStart))
		return ErrEmptyResponse
	}

	if !state.Completed {
		finishReason := "stop"
		if state.FunctionCallIndex != -1 {
			finishReason = "tool_calls"
		}
		synth := `{"id":"","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
		synth, _ = sjson.Set(synth, "id", state.ResponseID)
		synth, _ = sjson.Set(synth, "created", state.CreatedAt)
		synth, _ = sjson.Set(synth, "model", state.Model)
		synth, _ = sjson.Set(synth, "choices.0.finish_reason", finishReason)
		chunkCount++
		_, _ = w.Write(sseDataPrefix)
		_, _ = io.WriteString(w, synth)
		_, _ = w.Write(sseDataSuffix)
		if flush != nil {
			flush()
		}
	}

	doneWriteStart := time.Now()
	_, _ = w.Write(sseDoneMarker)
	if flush != nil {
		flush()
	}
	doneWriteDur := time.Since(doneWriteStart)

	if state.UsageInput > 0 || state.UsageOutput > 0 {
		s.account.RecordUsage(state.UsageInput, state.UsageOutput, state.UsageTotal)
	}
	s.account.RecordSuccess()
	firstChunkDur := time.Duration(0)
	completedDur := time.Duration(0)
	tailAfterCompleted := time.Duration(0)
	if !firstChunkAt.IsZero() {
		firstChunkDur = firstChunkAt.Sub(streamStart)
	}
	if !completedAt.IsZero() {
		completedDur = completedAt.Sub(streamStart)
		tailAfterCompleted = time.Since(completedAt)
	}
	log.Infof("req summary stream model=%s account=%s attempts=%d convert=%v upstream_ttfb=%v first_chunk=%v to_completed=%v tail_after_completed=%v done_write=%v stream=%v chunks=%d total=%v", s.BaseModel, s.account.GetEmail(), s.Attempts, s.ConvertDur, s.SendDur, firstChunkDur, completedDur, tailAfterCompleted, doneWriteDur, time.Since(streamStart), chunkCount, time.Since(streamStart))
	return nil
}

// PumpRawSSE 原样转发上游 SSE 字节（Responses API）。
func (s *CodexResponsesStream) PumpRawSSE(w io.Writer, flush func()) error {
	defer func() { _ = s.body.Close() }()
	buf := make([]byte, httpBufferSize)
	streamStart := time.Now()
	for {
		n, readErr := s.body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flush != nil {
				flush()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				s.account.RecordSuccess()
				log.Infof("req summary responses-stream model=%s account=%s attempts=%d convert=%v upstream=%v total=%v", s.BaseModel, s.account.GetEmail(), s.Attempts, s.ConvertDur, s.SendDur, time.Since(streamStart))
				return nil
			}
			if errors.Is(readErr, context.Canceled) {
				log.Infof("req summary responses-stream model=%s account=%s attempts=%d convert=%v upstream=%v total=%v (canceled)", s.BaseModel, s.account.GetEmail(), s.Attempts, s.ConvertDur, s.SendDur, time.Since(streamStart))
				return nil
			}
			log.Errorf("读取流式响应失败: %v", readErr)
			log.Infof("req summary responses-stream model=%s account=%s attempts=%d convert=%v upstream=%v total=%v (ERR)", s.BaseModel, s.account.GetEmail(), s.Attempts, s.ConvertDur, s.SendDur, time.Since(streamStart))
			return wrapReadErr(readErr)
		}
	}
}

// CodexCompactStream /responses/compact 成功后的响应（含待透传头与 Body）。
type CodexCompactStream struct {
	Resp       *http.Response
	Account    *auth.Account
	Attempts   int
	BaseModel  string
	ConvertDur time.Duration
	SendDur    time.Duration
}

// PumpBody 透传 compact 响应体；成功读完时由调用方 RecordSuccess。
func (s *CodexCompactStream) PumpBody(w io.Writer, flush func()) error {
	defer func() { _ = s.Resp.Body.Close() }()
	buf := make([]byte, httpBufferSize)
	for {
		n, err := s.Resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flush != nil {
				flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return wrapReadErr(err)
		}
	}
}
