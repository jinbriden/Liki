package message

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"im-server/internal/gateway"
	"im-server/internal/pkg/auth"
	"im-server/internal/pkg/response"
)

// WebSocket 数据包类型（NFR-09）
const (
	TypeSend = "message.send" // 客户端 → 服务端：发送消息
	TypeAck  = "message.ack"  // 服务端 → 发送消息的那个连接：写库成功或失败的确认
	TypeNew  = "message.new"  // 服务端 → 会话双方的所有在线连接：有新消息
)

// Handler 消息模块的接口层，同时处理 HTTP 请求和 WebSocket 数据包。
// 两者的地位一样：都只负责解析参数、调用 service、把结果和错误翻译成响应，不写业务逻辑。
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes 注册 HTTP 路由，全部需要登录
func (h *Handler) RegisterRoutes(_, protected *gin.RouterGroup) {
	protected.GET("/chats/:peerId/messages", h.listMessages)
}

// PacketRouter 网关注册数据包处理函数的能力，由 *gateway.Gateway 实现
type PacketRouter interface {
	Handle(typ string, h gateway.HandlerFunc)
}

// RegisterPackets 注册 WebSocket 数据包处理函数
func (h *Handler) RegisterPackets(r PacketRouter) {
	r.Handle(TypeSend, h.onSend)
}

// ---------- 请求 / 响应结构体 ----------

// messageResp 对外输出的消息。HTTP 接口和 WebSocket 推送共用同一个格式
type messageResp struct {
	ID             int64     `json:"id"`
	ConversationID int64     `json:"conversationId"`
	Seq            int64     `json:"seq"`
	SenderID       int64     `json:"senderId"`
	RecipientID    int64     `json:"recipientId"`
	ClientMsgID    string    `json:"clientMsgId"`
	Type           string    `json:"type"`
	Content        string    `json:"content"`
	CreatedAt      time.Time `json:"createdAt"`
}

func toMessageResp(m *Message) messageResp {
	return messageResp{
		ID:             m.ID,
		ConversationID: m.ConversationID,
		Seq:            m.Seq,
		SenderID:       m.SenderID,
		RecipientID:    m.RecipientID,
		ClientMsgID:    m.ClientMsgID,
		Type:           m.Type,
		Content:        m.Content,
		CreatedAt:      m.CreatedAt.UTC(),
	}
}

type pageResp struct {
	Items   []messageResp `json:"items"`
	HasMore bool          `json:"hasMore"`
}

type sendData struct {
	ClientMsgID string `json:"clientMsgId"`
	ToUserID    int64  `json:"toUserId"` // 接收方；发送方一律取连接上的用户 ID，数据包里不带
	Content     string `json:"content"`
}

// ackData message.ack 的内容。code 为 0 表示已写库（包括重复提交时返回已有的那条），message 带着服务端分配的 seq；
// 非 0 表示被拒绝，客户端把这条消息标记为发送失败，不要重发。
type ackData struct {
	ClientMsgID string       `json:"clientMsgId"`
	Code        int          `json:"code"`
	Msg         string       `json:"msg"`
	Message     *messageResp `json:"message,omitempty"`
}

// ---------- 错误处理 ----------

const (
	msgBadRequest     = "请求格式错误"
	msgEmptyContent   = "不能发送空白消息"
	msgContentTooLong = "消息过长，请重新发送" // FR-13：上限 1500 字
	msgInvalidContent = "消息包含不支持的字符"
	msgInvalidPeer    = "不能给自己发消息"
	msgNotFriends     = "对方不是你的好友，无法发送消息"
	msgInternal       = "服务器内部错误"
)

// errorCode 把 service 返回的错误翻译成 HTTP 状态码、错误码和提示。未知错误返回 500，由调用方记录日志。
func errorCode(err error) (status, code int, msg string) {
	switch {
	case errors.Is(err, ErrInvalidClientMsgID):
		return http.StatusBadRequest, response.CodeBadRequest, msgBadRequest // 前端代码的 bug
	case errors.Is(err, ErrEmptyContent):
		return http.StatusBadRequest, response.CodeMessageEmpty, msgEmptyContent
	case errors.Is(err, ErrContentTooLong):
		return http.StatusBadRequest, response.CodeMessageTooLong, msgContentTooLong
	case errors.Is(err, ErrInvalidContent):
		return http.StatusBadRequest, response.CodeInvalidParam, msgInvalidContent
	case errors.Is(err, ErrInvalidPeer):
		return http.StatusBadRequest, response.CodeInvalidParam, msgInvalidPeer
	case errors.Is(err, ErrNotFriends):
		return http.StatusForbidden, response.CodeNotFriends, msgNotFriends
	default:
		return http.StatusInternalServerError, response.CodeInternal, msgInternal
	}
}

// ---------- HTTP ----------

// listMessages GET /chats/:peerId/messages：我和 peerId 的会话中的消息，按 seq 升序。
//   - ?afterSeq=N：seq > N 的消息（增量同步，FR-17）。hasMore 为 true 时客户端以最后一条的 seq 继续拉。
//   - ?beforeSeq=N：seq < N 的最近一页（向上翻页，FR-18）；两个参数都不带时返回最新一页。
//   - ?limit=：每页条数，默认 50，最多 100。
func (h *Handler) listMessages(c *gin.Context) {
	peerID, err := strconv.ParseInt(c.Param("peerId"), 10, 64)
	if err != nil || peerID <= 0 {
		response.Fail(c, http.StatusBadRequest, response.CodeBadRequest, msgBadRequest)
		return
	}
	afterSeq, okA := queryInt(c, "afterSeq")
	beforeSeq, okB := queryInt(c, "beforeSeq")
	limit, okL := queryInt(c, "limit")
	if !okA || !okB || !okL || (c.Query("afterSeq") != "" && c.Query("beforeSeq") != "") {
		response.Fail(c, http.StatusBadRequest, response.CodeBadRequest, msgBadRequest)
		return
	}

	ctx := c.Request.Context()
	me := auth.UserID(c)
	var page *Page
	if c.Query("afterSeq") != "" {
		page, err = h.svc.ListAfter(ctx, me, peerID, afterSeq, int(limit))
	} else {
		page, err = h.svc.ListBefore(ctx, me, peerID, beforeSeq, int(limit))
	}
	if err != nil {
		status, code, msg := errorCode(err)
		if code == response.CodeInternal {
			slog.ErrorContext(ctx, "list messages failed", "peer_id", peerID, "err", err)
		}
		response.Fail(c, status, code, msg)
		return
	}

	items := make([]messageResp, 0, len(page.Messages))
	for i := range page.Messages {
		items = append(items, toMessageResp(&page.Messages[i]))
	}
	response.OK(c, pageResp{Items: items, HasMore: page.HasMore})
}

// queryInt 解析可选的非负整数查询参数。没传时返回 (0, true)，格式不对返回 ok=false
func queryInt(c *gin.Context, name string) (int64, bool) {
	s := c.Query(name)
	if s == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n >= 0
}

// ---------- WebSocket ----------

// onSend 处理 message.send：写库成功后回 ACK（带 seq），失败也回 ACK（带错误码）。
func (h *Handler) onSend(ctx context.Context, s gateway.Session, raw json.RawMessage) {
	var d sendData
	if err := json.Unmarshal(raw, &d); err != nil {
		// 连 clientMsgId 都解析不出来，没法回 ACK，只能回一个通用错误包
		s.Reply(ctx, gateway.TypeError, gateway.ErrorData{Code: response.CodeBadRequest, Msg: msgBadRequest, Type: TypeSend})
		return
	}

	res, err := h.svc.Send(ctx, s.UserID(), d.ToUserID, d.ClientMsgID, d.Content)
	if err != nil {
		_, code, msg := errorCode(err)
		if code == response.CodeInternal {
			// 服务端出错（如数据库不可用）：客户端收到 50000 后会稍后用同一个 clientMsgId 重发
			slog.ErrorContext(ctx, "send message failed", "client_msg_id", d.ClientMsgID, "err", err)
		} else {
			slog.InfoContext(ctx, "send message rejected", "client_msg_id", d.ClientMsgID, "to_user_id", d.ToUserID, "reason", err.Error())
		}
		s.Reply(ctx, TypeAck, ackData{ClientMsgID: d.ClientMsgID, Code: code, Msg: msg})
		return
	}

	m := toMessageResp(res.Message)
	s.Reply(ctx, TypeAck, ackData{ClientMsgID: d.ClientMsgID, Code: response.CodeOk, Msg: "ok", Message: &m})
}
