package message

import "context"

// Pusher 把数据包推送给某个用户的所有在线连接，由 *gateway.Gateway 实现。
// 按 Go 的习惯由使用方定义接口：消息模块只知道"能推送"，不知道 WebSocket 的存在。
type Pusher interface {
	Push(ctx context.Context, userID int64, typ string, data any)
}

// pushNotifier 用 Pusher 实现 Notifier：决定推送的数据包类型和格式（和 HTTP 接口共用 messageResp）
type pushNotifier struct {
	pusher Pusher
}

func NewNotifier(p Pusher) Notifier {
	return pushNotifier{pusher: p}
}

type newMessageData struct {
	Message messageResp `json:"message"`
}

func (n pushNotifier) MessageCreated(ctx context.Context, m *Message, recipients []int64) {
	data := newMessageData{Message: toMessageResp(m)}
	for _, uid := range recipients {
		n.pusher.Push(ctx, uid, TypeNew, data)
	}
}

// readData chat.read 的内容。peerId 标明是和谁的会话（前端按对方用户 ID 组织会话）
type readData struct {
	PeerID  int64 `json:"peerId"`
	ReadSeq int64 `json:"readSeq"`
}

// ReadUpdated 推给自己的所有连接，包括发起这次上报的那个标签页（HTTP 请求不知道来自哪个连接）。
// 客户端按"取较大值"合并，收到自己刚上报的位置也没有影响。
func (n pushNotifier) ReadUpdated(ctx context.Context, userID, peerID, readSeq int64) {
	n.pusher.Push(ctx, userID, TypeRead, readData{PeerID: peerID, ReadSeq: readSeq})
}
