package handler

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/fasthttp/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/valyala/fasthttp"
)

var accountsIngestWSUpgrader = websocket.FastHTTPUpgrader{
	ReadBufferSize:  wsBufferSize,
	WriteBufferSize: wsBufferSize,
	CheckOrigin: func(ctx *fasthttp.RequestCtx) bool {
		return true
	},
}

/**
 * handleAccountsIngest POST 请求体为单个对象、数组或 NDJSON；带 Upgrade: websocket 时升级为 WS，每帧一条同类 payload
 */
func (h *ProxyHandler) handleAccountsIngest(ctx *fasthttp.RequestCtx) {
	if isWebSocketUpgradeRequest(ctx) {
		h.handleAccountsIngestWS(ctx)
		return
	}
	if !ctx.IsPost() {
		writeJSON(ctx, fasthttp.StatusMethodNotAllowed, map[string]any{
			"error": map[string]string{"message": "请使用 POST 上传；WebSocket 使用带 Upgrade 的 GET 或 POST"},
		})
		return
	}
	body := ctx.PostBody()
	res, err := h.manager.IngestAccountsFromJSON(body)
	if err != nil {
		writeJSON(ctx, fasthttp.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error()},
		})
		return
	}
	writeJSON(ctx, fasthttp.StatusOK, res)
}

func (h *ProxyHandler) handleAccountsIngestWS(ctx *fasthttp.RequestCtx) {
	err := accountsIngestWSUpgrader.Upgrade(ctx, func(conn *websocket.Conn) {
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Time{})

		for {
			_, message, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			msg := bytes.TrimSpace(message)
			if len(msg) == 0 {
				continue
			}
			if bytesEqualFold(msg, []byte("ping")) {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"pong"}`))
				continue
			}
			res, ierr := h.manager.IngestAccountsFromJSON(msg)
			if ierr != nil {
				out := map[string]any{
					"ok":    false,
					"error": ierr.Error(),
				}
				b, _ := json.Marshal(out)
				_ = conn.WriteMessage(websocket.TextMessage, b)
				continue
			}
			b, mErr := json.Marshal(res)
			if mErr != nil {
				log.Warnf("accounts ingest ws: marshal: %v", mErr)
				continue
			}
			if wErr := conn.WriteMessage(websocket.TextMessage, b); wErr != nil {
				return
			}
		}
	})
	if err != nil {
		log.Warnf("accounts ingest ws upgrade 失败: %v", err)
	}
}
