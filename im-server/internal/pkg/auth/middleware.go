package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"im-server/internal/pkg/logger"
	"im-server/internal/pkg/response"
)

const ctxKeyUserID = "auth.userID"

// TokenParser 中间件只依赖"能解析令牌"这一能力。
// 以后如果要加令牌黑名单（如 Redis），包一层实现这个接口即可，中间件不用改。
type TokenParser interface {
	Parse(token string) (int64, error)
}

// Middleware 校验请求头 Authorization: Bearer <token>。
// 校验失败返回 401 并中止请求；成功则把用户 ID 写入上下文，供后续 handler 通过 UserID 读取。
func Middleware(p TokenParser) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			unauthorized(c)
			return
		}
		userID, err := p.Parse(token)
		if err != nil {
			unauthorized(c)
			return
		}
		c.Set(ctxKeyUserID, userID)
		// 把 user_id 挂到请求的 ctx 上：之后 service、repository 带着这个 ctx 打的日志（包括 SQL 日志）都会带上用户 ID
		c.Request = c.Request.WithContext(logger.WithAttrs(c.Request.Context(), "user_id", userID))
		c.Next()
	}
}

// UserID 返回当前请求的用户 ID（来自令牌，不信任客户端传入的任何用户 ID）。
// 只能在挂了 Middleware 的路由里使用；未经过中间件时返回 0。
func UserID(c *gin.Context) int64 {
	return c.GetInt64(ctxKeyUserID)
}

// bearerToken 从 "Bearer <token>" 中取出令牌，scheme 大小写不敏感（RFC 7235）
func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

func unauthorized(c *gin.Context) {
	response.Fail(c, http.StatusUnauthorized, response.CodeUnauthorized, "未登录或登录已过期")
	c.Abort() // 中止后续 handler，类似 Spring 拦截器 preHandle 返回 false
}
