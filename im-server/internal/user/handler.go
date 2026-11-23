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
	svc     *Service
	invites *InviteService
}

func NewHandler(svc *Service, invites *InviteService) *Handler {
	return &Handler{svc: svc, invites: invites}
}

// RegisterRoutes 注册用户相关的路由。
// public 下的接口无需登录；protected 已挂载鉴权中间件，handler 里可以用 auth.UserID 取当前用户。
func (h *Handler) RegisterRoutes(public, protected *gin.RouterGroup) {
	public.POST("/auth/register", h.register)
	public.POST("/auth/login", h.login)
	// 注册前验证邀请码（FR-23）：验证通过即冻结，注册页开着时续期，离开时解冻
	public.POST("/auth/invite/verify", h.verifyInvite)
	public.POST("/auth/invite/renew", h.renewInvite)
	public.POST("/auth/invite/release", h.releaseInvite)
	protected.GET("/me", h.me)
	// 邀请码（FR-23）：能不能生成由 InviteService.CanInvite 判断，不在路由上写死"只有管理员"
	protected.POST("/invites", h.createInvite)
	protected.GET("/invites", h.listInvites)
	protected.POST("/invites/:id/revoke", h.revokeInvite)
}

// handler 只负责 JSON 绑定，不写 binding 校验规则，业务规则统一由 service 校验。
type registerReq struct {
	Username         string `json:"username"`
	Password         string `json:"password"`
	InviteCode       string `json:"inviteCode"`
	ReservationToken string `json:"reservationToken"` // 验证邀请码时拿到的冻结令牌
}

type loginReq struct {
	UserID   int64  `json:"userId"`
	Password string `json:"password"`
}

// registerResp 注册结果，不包含 password_hash。status 固定是 pending（等待审核）
type registerResp struct {
	ID        int64         `json:"id"`
	Username  string        `json:"username"`
	Status    AccountStatus `json:"status"`
	CreatedAt time.Time     `json:"createdAt"`
}

// userResp 当前登录用户的信息（登录、/me 返回）。
// canInvite 告诉前端要不要显示"邀请"入口：返回能力而不是 isAdmin，以后权限规则变了前端不用改
type userResp struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"createdAt"`
	CanInvite bool      `json:"canInvite"`
}

type loginResp struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	User      userResp  `json:"user"`
}

// toUserResp 时间统一转成 UTC 输出（形如 2026-11-21T13:33:39.400657Z），不受服务器所在时区影响
func (h *Handler) toUserResp(u *User) userResp {
	return userResp{ID: u.ID, Username: u.Username, CreatedAt: u.CreatedAt.UTC(), CanInvite: h.invites.CanInvite(u.ID)}
}

const (
	msgBadRequest         = "请求格式错误"
	msgInvalidUsername    = "用户名需为 3-32 位英文字母或数字"
	msgInvalidPassword    = "密码需为 8-24 位，至少包含一个大写字母、一个小写字母和一个数字，只能使用英文字母、数字和英文符号"
	msgInvalidCredentials = "用户 ID 或密码错误"
	msgTooManyAttempts    = "登录尝试太频繁，请稍后再试"
	msgUnauthorized       = "未登录或登录已过期"
	msgAccountPending     = "账号正在等待管理员审核，审核通过后才能登录"
	msgAccountRejected    = "注册申请未通过审核"
	msgInvalidInvite      = "邀请码无效或已失效，请向邀请人重新获取"
	msgReservationLost    = "邀请码的验证已失效，请重新验证"
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

	u, err := h.svc.Register(c.Request.Context(), req.Username, req.Password, req.InviteCode, req.ReservationToken)
	switch {
	case errors.Is(err, ErrInvalidUsername):
		response.Fail(c, http.StatusBadRequest, response.CodeInvalidParam, msgInvalidUsername)
		return
	case errors.Is(err, ErrInvalidPassword):
		response.Fail(c, http.StatusBadRequest, response.CodeInvalidParam, msgInvalidPassword)
		return
	case errors.Is(err, ErrReservationLost):
		response.Fail(c, http.StatusConflict, response.CodeReservationLost, msgReservationLost)
		return
	case err != nil:
		slog.ErrorContext(c.Request.Context(), "register failed", "err", err)
		response.Fail(c, http.StatusInternalServerError, response.CodeInternal, msgInternal)
		return
	}

	response.OK(c, registerResp{ID: u.ID, Username: u.Username, Status: u.Status, CreatedAt: u.CreatedAt.UTC()})
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
	case errors.Is(err, ErrTooManyAttempts):
		response.Fail(c, http.StatusTooManyRequests, response.CodeTooManyRequests, msgTooManyAttempts)
		return
	// 403：身份已经确认（密码正确），只是账号还不允许使用；和 401 区分开，前端也不会当成令牌失效
	case errors.Is(err, ErrAccountPending):
		response.Fail(c, http.StatusForbidden, response.CodeAccountPending, msgAccountPending)
		return
	case errors.Is(err, ErrAccountRejected):
		response.Fail(c, http.StatusForbidden, response.CodeAccountRejected, msgAccountRejected)
		return
	case err != nil:
		slog.ErrorContext(c.Request.Context(), "login failed", "login_user_id", req.UserID, "err", err) // 不记录密码和令牌
		response.Fail(c, http.StatusInternalServerError, response.CodeInternal, msgInternal)
		return
	}

	response.OK(c, loginResp{
		Token:     res.Token,
		ExpiresAt: res.ExpiresAt.UTC(),
		User:      h.toUserResp(res.User),
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
		slog.ErrorContext(c.Request.Context(), "get current user failed", "err", err) // user_id 已由鉴权中间件挂在 ctx 上
		response.Fail(c, http.StatusInternalServerError, response.CodeInternal, msgInternal)
		return
	}

	response.OK(c, h.toUserResp(u))
}
