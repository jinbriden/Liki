package friend

import "context"

// WebSocket 数据包类型（NFR-09）
const (
	TypeRequestReceived = "friend.request.received" // 收到新的好友申请，data 是申请（user 为申请人）
	TypeRequestHandled  = "friend.request.handled"  // 我发出的申请被处理了，data 是申请（user 为处理人，status 为结果）
	TypeSync            = "friend.sync"             // 我的好友数据在别处变了，客户端重新拉取好友和申请列表
)

// Pusher 把数据包推送给某个用户的所有在线连接，由 *gateway.Gateway 实现
type Pusher interface {
	Push(ctx context.Context, userID int64, typ string, data any)
}

// pushNotifier 用 Pusher 实现 Notifier，推送的申请格式和 HTTP 接口的 friendRequestResp 一致
type pushNotifier struct {
	pusher Pusher
}

func NewNotifier(p Pusher) Notifier {
	return pushNotifier{pusher: p}
}

func (n pushNotifier) RequestReceived(ctx context.Context, toUserID int64, view RequestView) {
	n.pusher.Push(ctx, toUserID, TypeRequestReceived, toFriendRequestResp(view))
}

func (n pushNotifier) RequestHandled(ctx context.Context, fromUserID int64, view RequestView) {
	n.pusher.Push(ctx, fromUserID, TypeRequestHandled, toFriendRequestResp(view))
}

func (n pushNotifier) FriendDataChanged(ctx context.Context, userID int64) {
	n.pusher.Push(ctx, userID, TypeSync, nil)
}
