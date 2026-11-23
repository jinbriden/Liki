package user

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"im-server/internal/pkg/auth"
	"im-server/internal/pkg/response"
)

// 邀请码接口（FR-23）。
//   - 验证、续期、解冻：注册页使用，无需登录
//   - 生成、列表、作废：需要登录，能不能生成由 InviteService.CanInvite 判断。网页上只能用默认有效期，自定义只在命令行里提供

type inviteCodeReq struct {
	InviteCode string `json:"inviteCode"`
}

type reservationReq struct {
	InviteCode       string `json:"inviteCode"`
	ReservationToken string `json:"reservationToken"`
	// Soft 软释放（只保留 15 秒）：页面卸载时用，因为分不清是刷新还是关闭标签页
	Soft bool `json:"soft"`
}

// reservationResp 冻结信息。reservationToken 只在验证成功时返回
type reservationResp struct {
	ReservationToken string    `json:"reservationToken,omitempty"`
	ExpiresAt        time.Time `json:"expiresAt"`     // 邀请码本身的过期时间，冻结不会延长它
	ReservedUntil    time.Time `json:"reservedUntil"` // 冻结到什么时候（之后靠续期）
}

type createInviteReq struct {
	Note string `json:"note"`
}

// inviteeResp 用某个邀请码提交注册申请的账号（溯源）
type inviteeResp struct {
	ID        int64         `json:"id"`
	Username  string        `json:"username"`
	Status    AccountStatus `json:"status"`
	CreatedAt time.Time     `json:"createdAt"`
}

// verificationResp 一次验证记录（溯源）
type verificationResp struct {
	IP        string       `json:"ip"`
	Result    VerifyResult `json:"result"`
	CreatedAt time.Time    `json:"createdAt"`
}

type inviteResp struct {
	ID            int64              `json:"id"`
	Code          string             `json:"code"`
	Note          string             `json:"note"`
	State         InviteState        `json:"state"`
	ExpiresAt     time.Time          `json:"expiresAt"`
	UsedAt        *time.Time         `json:"usedAt"`        // 没用过为 null
	RevokedAt     *time.Time         `json:"revokedAt"`     // 没作废为 null
	ReservedUntil *time.Time         `json:"reservedUntil"` // 只在冻结中时有值
	CreatedAt     time.Time          `json:"createdAt"`
	UsedBy        *inviteeResp       `json:"usedBy"`        // 没用过为 null
	Verifications []verificationResp `json:"verifications"` // 一定是数组（可能为空），前端不用判断 null
}

type inviteListResp struct {
	Items []inviteResp `json:"items"`
}

const (
	msgInviteInUse          = "该邀请码正在被使用，请稍后再试"
	msgVerifyTooFrequent    = "该邀请码 1 分钟内只能验证一次，请稍后再试"
	msgVerifyCodeDailyLimit = "该邀请码今天的验证次数已用完，请明天再试或联系邀请人"
	msgVerifyIPLimit        = "验证次数太多，请 24 小时后再试"
	msgInviteForbidden      = "你没有生成邀请码的权限"
	msgInvalidInviteNote    = "备注最多 50 个字符"
	msgInvalidInviteOptions = "邀请码的有效期超出允许范围"
	msgInviteNotFound       = "邀请码不存在"
)

// utcPtr 可空时间转成 UTC；nil 原样返回
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func toInviteResp(d *InviteDetail, now time.Time) inviteResp {
	state := d.StateAt(now)
	resp := inviteResp{
		ID:            d.ID,
		Code:          d.Code,
		Note:          d.Note,
		State:         state,
		ExpiresAt:     d.ExpiresAt.UTC(),
		UsedAt:        utcPtr(d.UsedAt),
		RevokedAt:     utcPtr(d.RevokedAt),
		CreatedAt:     d.CreatedAt.UTC(),
		Verifications: make([]verificationResp, 0, len(d.Verifications)),
	}
	// 冻结到期或解冻后，库里可能还留着旧的冻结时间，只在确实冻结中时返回
	if state == InviteReserved {
		resp.ReservedUntil = utcPtr(d.ReservedUntil)
	}
	if u := d.UsedBy; u != nil {
		resp.UsedBy = &inviteeResp{ID: u.ID, Username: u.Username, Status: u.Status, CreatedAt: u.CreatedAt.UTC()}
	}
	for _, v := range d.Verifications {
		resp.Verifications = append(resp.Verifications, verificationResp{IP: v.IP, Result: v.Result, CreatedAt: v.CreatedAt.UTC()})
	}
	return resp
}

func toReservationResp(r *Reservation) reservationResp {
	return reservationResp{ReservationToken: r.Token, ExpiresAt: r.ExpiresAt.UTC(), ReservedUntil: r.ReservedUntil.UTC()}
}

// failInvite 把邀请码相关的业务错误翻译成错误码；未知错误记 Error 并返回 50000
func failInvite(c *gin.Context, action string, err error) {
	switch {
	case errors.Is(err, ErrInvalidInvite):
		response.Fail(c, http.StatusBadRequest, response.CodeInvalidInvite, msgInvalidInvite)
	case errors.Is(err, ErrInviteInUse):
		response.Fail(c, http.StatusConflict, response.CodeInviteInUse, msgInviteInUse)
	case errors.Is(err, ErrReservationLost):
		response.Fail(c, http.StatusConflict, response.CodeReservationLost, msgReservationLost)
	case errors.Is(err, ErrInviteVerifyTooFrequent):
		response.Fail(c, http.StatusTooManyRequests, response.CodeTooManyRequests, msgVerifyTooFrequent)
	case errors.Is(err, ErrInviteVerifyDailyLimit):
		response.Fail(c, http.StatusTooManyRequests, response.CodeTooManyRequests, msgVerifyCodeDailyLimit)
	case errors.Is(err, ErrVerifyIPLimit):
		response.Fail(c, http.StatusTooManyRequests, response.CodeTooManyRequests, msgVerifyIPLimit)
	case errors.Is(err, ErrInviteForbidden):
		response.Fail(c, http.StatusForbidden, response.CodeForbidden, msgInviteForbidden)
	case errors.Is(err, ErrInvalidInviteNote):
		response.Fail(c, http.StatusBadRequest, response.CodeInvalidParam, msgInvalidInviteNote)
	case errors.Is(err, ErrInvalidInviteOptions):
		response.Fail(c, http.StatusBadRequest, response.CodeInvalidParam, msgInvalidInviteOptions)
	case errors.Is(err, ErrInviteNotFound):
		response.Fail(c, http.StatusNotFound, response.CodeInviteNotFound, msgInviteNotFound)
	default:
		slog.ErrorContext(c.Request.Context(), action+" failed", "err", err)
		response.Fail(c, http.StatusInternalServerError, response.CodeInternal, msgInternal)
	}
}

// bindJSON 绑定请求体，失败时写 10002 并返回 false
func bindJSON(c *gin.Context, req any) bool {
	if err := c.ShouldBindJSON(req); err != nil {
		response.Fail(c, http.StatusBadRequest, response.CodeBadRequest, msgBadRequest)
		return false
	}
	return true
}

// ---------- 注册页：验证、续期、解冻 ----------

func (h *Handler) verifyInvite(c *gin.Context) {
	var req inviteCodeReq
	if !bindJSON(c, &req) {
		return
	}
	// c.ClientIP()：线上在 Nginx 后面时依赖 TRUSTED_PROXIES 配置，否则所有人都是 127.0.0.1，共用一份 IP 限额
	r, err := h.invites.Verify(c.Request.Context(), req.InviteCode, c.ClientIP())
	if err != nil {
		failInvite(c, "verify invite", err)
		return
	}
	response.OK(c, toReservationResp(r))
}

func (h *Handler) renewInvite(c *gin.Context) {
	var req reservationReq
	if !bindJSON(c, &req) {
		return
	}
	r, err := h.invites.Renew(c.Request.Context(), req.InviteCode, req.ReservationToken)
	if err != nil {
		failInvite(c, "renew invite reservation", err)
		return
	}
	resp := toReservationResp(r)
	resp.ReservationToken = "" // 令牌只在验证时返回一次
	response.OK(c, resp)
}

func (h *Handler) releaseInvite(c *gin.Context) {
	var req reservationReq
	if !bindJSON(c, &req) {
		return
	}
	if err := h.invites.Release(c.Request.Context(), req.InviteCode, req.ReservationToken, req.Soft); err != nil {
		failInvite(c, "release invite reservation", err)
		return
	}
	response.OK(c, nil)
}

// ---------- 管理：生成、列表、作废 ----------

func (h *Handler) createInvite(c *gin.Context) {
	var req createInviteReq
	// 请求体可以为空（不填备注）：只有带了请求体但解析失败时才算 10002
	if c.Request.ContentLength != 0 && !bindJSON(c, &req) {
		return
	}
	code, err := h.invites.Create(c.Request.Context(), auth.UserID(c), InviteOptions{Note: req.Note})
	if err != nil {
		failInvite(c, "create invite", err)
		return
	}
	// 新码还没有人验证、使用过，不用再查一次
	response.OK(c, toInviteResp(&InviteDetail{InviteCode: *code}, time.Now()))
}

func (h *Handler) listInvites(c *gin.Context) {
	details, err := h.invites.List(c.Request.Context(), auth.UserID(c))
	if err != nil {
		failInvite(c, "list invites", err)
		return
	}
	now := time.Now()
	items := make([]inviteResp, 0, len(details))
	for i := range details {
		items = append(items, toInviteResp(&details[i], now))
	}
	response.OK(c, inviteListResp{Items: items})
}

func (h *Handler) revokeInvite(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Fail(c, http.StatusBadRequest, response.CodeBadRequest, msgBadRequest)
		return
	}
	d, err := h.invites.Revoke(c.Request.Context(), auth.UserID(c), id)
	if err != nil {
		failInvite(c, "revoke invite", err)
		return
	}
	response.OK(c, toInviteResp(d, time.Now()))
}
