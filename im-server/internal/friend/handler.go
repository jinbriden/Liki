package friend

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"im-server/internal/pkg/auth"
	"im-server/internal/pkg/response"
	"im-server/internal/user"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes 注册好友相关的路由，全部需要登录。public 参数保留，和其他模块的签名一致。
func (h *Handler) RegisterRoutes(_, protected *gin.RouterGroup) {
	protected.GET("/friends/search", h.search)
	protected.GET("/friends", h.listFriends)
	protected.DELETE("/friends/:friendId", h.deleteFriend)

	protected.POST("/friend-requests", h.sendRequest)
	protected.GET("/friend-requests/incoming", h.listIncoming)
	protected.GET("/friend-requests/outgoing", h.listOutgoing)
	protected.POST("/friend-requests/:id/accept", h.accept)
	protected.POST("/friend-requests/:id/reject", h.reject)
}

// ---------- 请求 / 响应结构体 ----------

type sendRequestReq struct {
	ToUserID int64  `json:"toUserId"`
	Message  string `json:"message"`
}

// profileResp 对外返回的用户资料
type profileResp struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatarUrl"`
}

// searchItemResp 嵌入 profileResp（匿名字段），JSON 序列化时字段会被"拍平"：
// {"id":..,"username":..,"avatarUrl":..,"relation":..}。类似继承，但 Go 里是组合。
type searchItemResp struct {
	profileResp
	Relation Relation `json:"relation"`
}

type friendResp struct {
	profileResp
	Since time.Time `json:"since"`
}

type friendRequestResp struct {
	ID        int64         `json:"id"`
	User      profileResp   `json:"user"` // 对方：收到的申请里是申请人，发出的申请里是被申请人
	Message   string        `json:"message"`
	Status    RequestStatus `json:"status"`
	CreatedAt time.Time     `json:"createdAt"`
	HandledAt *time.Time    `json:"handledAt,omitempty"`
}

type sendRequestResp struct {
	ID        int64         `json:"id"`
	ToUserID  int64         `json:"toUserId"`
	Message   string        `json:"message"`
	Status    RequestStatus `json:"status"`
	CreatedAt time.Time     `json:"createdAt"`
}

// listResp 列表统一包一层 items，以后加分页字段（如 nextCursor）时不破坏格式
type listResp[T any] struct {
	Items []T `json:"items"`
}

func toProfileResp(p user.Profile) profileResp {
	return profileResp{ID: p.ID, Username: p.Username, AvatarURL: p.AvatarURL}
}

func toFriendRequestResp(v RequestView) friendRequestResp {
	r := v.Request
	resp := friendRequestResp{
		ID:        r.ID,
		User:      toProfileResp(v.Peer),
		Message:   r.Message,
		Status:    r.Status,
		CreatedAt: r.CreatedAt.UTC(),
	}
	if r.HandledAt != nil {
		t := r.HandledAt.UTC()
		resp.HandledAt = &t
	}
	return resp
}

// mapSlice 把 []S 转成 []T。结果至少是空切片，序列化为 []，而不是 nil 切片序列化出的 null。
func mapSlice[S, T any](src []S, f func(S) T) []T {
	dst := make([]T, 0, len(src))
	for _, s := range src {
		dst = append(dst, f(s))
	}
	return dst
}

// ---------- 错误处理 ----------

const (
	msgBadRequest            = "请求格式错误"
	msgInvalidKeyword        = "请输入用户名或用户 ID"
	msgMessageTooLong        = "验证信息不能超过 100 个字"
	msgCannotAddSelf         = "不能添加自己为好友"
	msgAlreadyFriends        = "你们已经是好友了"
	msgReverseRequestPending = "对方已向你发送好友申请，请在申请列表中处理"
	msgRequestNotFound       = "好友申请不存在"
	msgRequestHandled        = "该申请已被处理"
	msgTargetUserNotFound    = "用户不存在"
	msgInternal              = "服务器内部错误"
)

// fail 把 service 返回的错误翻译成 HTTP 状态码、错误码和提示。action 只用于日志。
func fail(c *gin.Context, action string, err error) {
	switch {
	case errors.Is(err, ErrInvalidKeyword):
		response.Fail(c, http.StatusBadRequest, response.CodeInvalidParam, msgInvalidKeyword)
	case errors.Is(err, ErrMessageTooLong):
		response.Fail(c, http.StatusBadRequest, response.CodeInvalidParam, msgMessageTooLong)
	case errors.Is(err, ErrCannotAddSelf):
		response.Fail(c, http.StatusBadRequest, response.CodeCannotAddSelf, msgCannotAddSelf)
	case errors.Is(err, ErrAlreadyFriends):
		response.Fail(c, http.StatusConflict, response.CodeAlreadyFriends, msgAlreadyFriends)
	case errors.Is(err, ErrReverseRequestPending):
		response.Fail(c, http.StatusConflict, response.CodeReverseRequestPending, msgReverseRequestPending)
	case errors.Is(err, ErrRequestNotFound):
		response.Fail(c, http.StatusNotFound, response.CodeRequestNotFound, msgRequestNotFound)
	case errors.Is(err, ErrRequestNotPending):
		response.Fail(c, http.StatusConflict, response.CodeRequestHandled, msgRequestHandled)
	case errors.Is(err, ErrTargetUserNotFound):
		response.Fail(c, http.StatusNotFound, response.CodeTargetUserNotFound, msgTargetUserNotFound)
	default:
		slog.ErrorContext(c.Request.Context(), action+" failed", "err", err) // ctx 上已有 request_id、user_id
		response.Fail(c, http.StatusInternalServerError, response.CodeInternal, msgInternal)
	}
}

// pathID 解析路径参数里的 ID，不是合法正整数时写 10002 并返回 ok=false
func pathID(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		response.Fail(c, http.StatusBadRequest, response.CodeBadRequest, msgBadRequest)
		return 0, false
	}
	return id, true
}

// ---------- handler ----------

func (h *Handler) search(c *gin.Context) {
	results, err := h.svc.Search(c.Request.Context(), auth.UserID(c), c.Query("q"))
	if err != nil {
		fail(c, "search users", err)
		return
	}
	response.OK(c, listResp[searchItemResp]{Items: mapSlice(results, func(r SearchResult) searchItemResp {
		return searchItemResp{profileResp: toProfileResp(r.User), Relation: r.Relation}
	})})
}

func (h *Handler) sendRequest(c *gin.Context) {
	var req sendRequestReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, response.CodeBadRequest, msgBadRequest)
		return
	}

	r, err := h.svc.SendRequest(c.Request.Context(), auth.UserID(c), req.ToUserID, req.Message)
	if err != nil {
		fail(c, "send friend request", err)
		return
	}
	response.OK(c, sendRequestResp{
		ID:        r.ID,
		ToUserID:  r.ToUserID,
		Message:   r.Message,
		Status:    r.Status,
		CreatedAt: r.CreatedAt.UTC(),
	})
}

func (h *Handler) listIncoming(c *gin.Context) {
	views, err := h.svc.ListIncoming(c.Request.Context(), auth.UserID(c))
	if err != nil {
		fail(c, "list incoming requests", err)
		return
	}
	response.OK(c, listResp[friendRequestResp]{Items: mapSlice(views, toFriendRequestResp)})
}

func (h *Handler) listOutgoing(c *gin.Context) {
	views, err := h.svc.ListOutgoing(c.Request.Context(), auth.UserID(c))
	if err != nil {
		fail(c, "list outgoing requests", err)
		return
	}
	response.OK(c, listResp[friendRequestResp]{Items: mapSlice(views, toFriendRequestResp)})
}

func (h *Handler) accept(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	if err := h.svc.Accept(c.Request.Context(), auth.UserID(c), id); err != nil {
		fail(c, "accept friend request", err)
		return
	}
	response.OK(c, nil)
}

func (h *Handler) reject(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	if err := h.svc.Reject(c.Request.Context(), auth.UserID(c), id); err != nil {
		fail(c, "reject friend request", err)
		return
	}
	response.OK(c, nil)
}

func (h *Handler) listFriends(c *gin.Context) {
	friends, err := h.svc.ListFriends(c.Request.Context(), auth.UserID(c))
	if err != nil {
		fail(c, "list friends", err)
		return
	}
	response.OK(c, listResp[friendResp]{Items: mapSlice(friends, func(f Friend) friendResp {
		return friendResp{profileResp: toProfileResp(f.User), Since: f.Since.UTC()}
	})})
}

func (h *Handler) deleteFriend(c *gin.Context) {
	friendID, ok := pathID(c, "friendId")
	if !ok {
		return
	}
	if err := h.svc.DeleteFriend(c.Request.Context(), auth.UserID(c), friendID); err != nil {
		fail(c, "delete friend", err)
		return
	}
	response.OK(c, nil)
}
