package logger

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"

	"im-server/internal/pkg/response"
)

// HeaderRequestID 响应头里的请求 ID。前端或用户反馈问题时带上它，就能在日志里搜到这次请求的全部记录。
const HeaderRequestID = "X-Request-ID"

// Middleware 为每个请求生成请求 ID、挂到 ctx 上，并在请求结束后记一条访问日志。
// 要放在所有中间件的最前面，这样后面的中间件和 handler 打的日志都能带上 request_id。
//
// 访问日志只记录路径，不记录查询参数（WebSocket 票据就在查询参数里），也不记录请求头和请求体。
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		reqID := newRequestID()
		c.Header(HeaderRequestID, reqID)
		c.Request = c.Request.WithContext(WithAttrs(c.Request.Context(), "request_id", reqID))

		c.Next()

		status := c.Writer.Status()
		if c.IsWebsocket() && status == http.StatusOK {
			// 升级成功后连接被网关接管（Hijack），握手的 101 不经过 gin 的 ResponseWriter，这里看到的还是默认的 200
			status = http.StatusSwitchingProtocols
		}
		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}

		attrs := []any{
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"route", c.FullPath(), // 路由模板，如 /api/v1/friends/:friendId，便于按接口聚合
			"status", status,
			"latency_ms", float64(time.Since(start).Microseconds()) / 1000,
			"client_ip", c.ClientIP(),
		}
		if len(c.Errors) > 0 {
			attrs = append(attrs, "errors", c.Errors.String())
		}
		// 用 c.Request.Context()：鉴权中间件会把 user_id 挂到它上面，这里自动带出
		slog.Log(c.Request.Context(), level, "http request", attrs...)
	}
}

// Recovery 捕获 handler 里的 panic，记录堆栈并返回 50000，避免整个进程崩溃。
// 作用和 gin.Recovery() 一样，区别是日志走 slog（带 request_id），响应走统一格式。
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			// http.ErrAbortHandler 是标准库约定的"主动中止请求"，不当作错误，继续往上抛
			if err, ok := r.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(r)
			}
			slog.ErrorContext(c.Request.Context(), "panic recovered",
				"panic", r,
				"stack", string(debug.Stack()),
			)
			if !c.Writer.Written() {
				response.Fail(c, http.StatusInternalServerError, response.CodeInternal, "服务器内部错误")
			}
			c.Abort()
		}()
		c.Next()
	}
}

// newRequestID 生成 16 位十六进制的随机 ID。不沿用客户端传来的 X-Request-ID，防止伪造或注入奇怪的字符。
func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read 按文档不会返回错误
	return hex.EncodeToString(b)
}
