package user

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"im-server/internal/pkg/auth"
	"im-server/internal/pkg/response"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes 注册用户相关的路由。
// public 下的接口无需登录；protected 已挂载鉴权中间件，handler 里可以用 auth.UserID 取当前用户。
func (h *Handler) RegisterRoutes(public, protected *gin.RouterGroup) {
	public.POST("/auth/register", h.register)
	public.POST("/auth/login", h.login)
	protected.GET("/me", h.me)
}

// handler 只负责 JSON 绑定，不写 binding 校验规则，业务规则统一由 service 校验。
type registerReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginReq struct {
	UserID   int64  `json:"userId"`
	Password string `json:"password"`
}

// userResp 对外返回的用户信息，不包含 password_hash
type userResp struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"createdAt"`
}

type loginResp struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	User      userResp  `json:"user"`
}

// toUserResp 时间统一转成 UTC 输出（形如 2026-11-21T13:33:39.400657Z），不受服务器所在时区影响
func toUserResp(u *User) userResp {
	return userResp{ID: u.ID, Username: u.Username, CreatedAt: u.CreatedAt.UTC()}
}

const (
	msgBadRequest         = "请求格式错误"
	msgInvalidUsername    = "用户名需为 3-32 位英文字母或数字"
	msgInvalidPassword    = "密码需为 8-24 位，至少包含一个大写字母、一个小写字母和一个数字，只能使用英文字母、数字和英文符号"
	msgInvalidCredentials = "用户 ID 或密码错误"
	msgUnauthorized       = "未登录或登录已过期"
	msgInternal           = "服务器内部错误"
)

func (h *Handler) register(c *gin.Context) {
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		// 这里的错误只可能是请求体无法解析（空 body、JSON 语法错误、类型不匹配等）。
		// 不把 err.Error() 返回给客户端，里面含有 Go 结构体名、类型名等内部信息。
		response.Fail(c, http.StatusBadRequest, response.CodeBadRequest, msgBadRequest)
		return
	}

	u, err := h.svc.Register(c.Request.Context(), req.Username, req.Password)
	switch {
	case errors.Is(err, ErrInvalidUsername):
		response.Fail(c, http.StatusBadRequest, response.CodeInvalidParam, msgInvalidUsername)
		return
	case errors.Is(err, ErrInvalidPassword):
		response.Fail(c, http.StatusBadRequest, response.CodeInvalidParam, msgInvalidPassword)
		return
	case err != nil:
		slog.Error("register failed", "err", err)
		response.Fail(c, http.StatusInternalServerError, response.CodeInternal, msgInternal)
		return
	}

	response.OK(c, toUserResp(u))
}

func (h *Handler) login(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, response.CodeBadRequest, msgBadRequest)
		return
	}

	res, err := h.svc.Login(c.Request.Context(), req.UserID, req.Password)
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		response.Fail(c, http.StatusUnauthorized, response.CodeInvalidCredentials, msgInvalidCredentials)
		return
	case err != nil:
		slog.Error("login failed", "userId", req.UserID, "err", err) // 不记录密码和令牌
		response.Fail(c, http.StatusInternalServerError, response.CodeInternal, msgInternal)
		return
	}

	response.OK(c, loginResp{
		Token:     res.Token,
		ExpiresAt: res.ExpiresAt.UTC(),
		User:      toUserResp(res.User),
	})
}

func (h *Handler) me(c *gin.Context) {
	userID := auth.UserID(c)
	u, err := h.svc.GetByID(c.Request.Context(), userID)
	switch {
	case errors.Is(err, ErrUserNotFound):
		// 令牌有效但账号已不存在，按未登录处理，让前端重新登录
		response.Fail(c, http.StatusUnauthorized, response.CodeUnauthorized, msgUnauthorized)
		return
	case err != nil:
		slog.Error("get current user failed", "userId", userID, "err", err)
		response.Fail(c, http.StatusInternalServerError, response.CodeInternal, msgInternal)
		return
	}

	response.OK(c, toUserResp(u))
}
