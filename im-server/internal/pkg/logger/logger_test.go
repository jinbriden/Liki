package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// captureDefault 把默认 logger 换成写入 buf 的 JSON logger（带 contextHandler），测试结束后恢复
func captureDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(contextHandler{slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})}))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(buf)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode log line: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func TestWithAttrs(t *testing.T) {
	buf := captureDefault(t)

	parent := WithAttrs(context.Background(), "request_id", "r1")
	child := WithAttrs(parent, "user_id", int64(42))

	slog.InfoContext(child, "child")
	slog.InfoContext(parent, "parent")
	slog.Info("no ctx")

	lines := decodeLines(t, buf)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if lines[0]["request_id"] != "r1" || lines[0]["user_id"] != float64(42) {
		t.Errorf("child line = %v, want request_id and user_id", lines[0])
	}
	// 子 ctx 追加字段不能影响父 ctx
	if lines[1]["request_id"] != "r1" || lines[1]["user_id"] != nil {
		t.Errorf("parent line = %v, want only request_id", lines[1])
	}
	if lines[2]["request_id"] != nil {
		t.Errorf("line without ctx = %v, want no request_id", lines[2])
	}
}

func TestMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	buf := captureDefault(t)

	r := gin.New()
	r.Use(Middleware(), Recovery())
	r.GET("/ok", func(c *gin.Context) {
		slog.InfoContext(c.Request.Context(), "in handler")
		c.Status(http.StatusNoContent)
	})
	r.GET("/panic", func(*gin.Context) { panic("boom") })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ok?ticket=secret", nil))
	reqID := w.Header().Get(HeaderRequestID)
	if len(reqID) != 16 {
		t.Fatalf("X-Request-ID = %q, want 16 hex chars", reqID)
	}

	lines := decodeLines(t, buf)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %v", len(lines), lines)
	}
	if lines[0]["request_id"] != reqID {
		t.Errorf("handler log request_id = %v, want %s", lines[0]["request_id"], reqID)
	}
	access := lines[1]
	if access["msg"] != "http request" || access["status"] != float64(204) || access["request_id"] != reqID {
		t.Errorf("access log = %v", access)
	}
	if access["path"] != "/ok" {
		t.Errorf("access log path = %v, want /ok (query must not be logged)", access["path"])
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("panic status = %d, want 500", w.Code)
	}
	lines = decodeLines(t, buf)
	if len(lines) != 2 || lines[0]["msg"] != "panic recovered" || lines[1]["level"] != "ERROR" {
		t.Errorf("panic logs = %v", lines)
	}
}
