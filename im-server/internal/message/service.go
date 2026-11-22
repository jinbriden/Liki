package message

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode/utf8"
)

const (
	// MaxContentLen 单条消息最多 1500 个字符（按 rune 计数，FR-13），和库里的 CHECK 约束一致
	MaxContentLen = 1500
	// maxClientMsgIDLen client_msg_id 最长 64 个字符，和库里的 VARCHAR(64) 一致。前端用 UUID（36 个字符）
	maxClientMsgIDLen = 64

	DefaultPageSize = 50
	MaxPageSize     = 100
)

// FriendChecker 消息模块需要的好友关系查询能力，由 *friend.Service 实现。
// M4 阶段只允许好友之间发消息；M6 再按 FR-12 放宽为"非好友最多发一条"。
type FriendChecker interface {
	AreFriends(ctx context.Context, a, b int64) (bool, error)
}

// Notifier 新消息写库成功后，通知相关用户（实时推送）。
// service 只决定"通知谁"，推送的数据格式和怎么送达由实现决定（见 notify.go），service 不依赖 WebSocket。
// 推送是尽力而为的，不返回错误：接收方没收到也没关系，重连后会按 seq 增量同步补齐。
type Notifier interface {
	MessageCreated(ctx context.Context, m *Message, recipients []int64)
}

type Service struct {
	repo     Repository
	friends  FriendChecker
	notifier Notifier
}

func NewService(repo Repository, friends FriendChecker, notifier Notifier) *Service {
	return &Service{repo: repo, friends: friends, notifier: notifier}
}

// SendResult 发送结果。Duplicate 为 true 表示这条消息之前已经写过（客户端重发），返回的是已有的那条。
type SendResult struct {
	Message   *Message
	Duplicate bool
}

// Send senderID 给 peerID 发一条文字消息（FR-10、FR-11、FR-13）。
//
// 流程：校验 → 按 client_msg_id 去重 → 检查好友关系 → 找到（或创建）会话 → 事务内分配 seq 并写库 → 推送。
// 重复提交（同一个 sender_id + client_msg_id）时不重复写库，直接返回已有消息和它的 seq，客户端据此确认。
func (s *Service) Send(ctx context.Context, senderID, peerID int64, clientMsgID, content string) (*SendResult, error) {
	if err := validateClientMsgID(clientMsgID); err != nil {
		return nil, err
	}
	if err := validateContent(content); err != nil {
		return nil, err
	}
	if peerID <= 0 || peerID == senderID {
		return nil, ErrInvalidPeer
	}

	// 先去重再查好友：消息已经成功写过、只是 ACK 丢了，那么重发时即使双方刚好解除了好友，
	// 也应该告诉客户端"已发送"，而不是"发送失败"。
	if existing, err := s.findExisting(ctx, senderID, clientMsgID); err != nil || existing != nil {
		return existing, err
	}

	ok, err := s.friends.AreFriends(ctx, senderID, peerID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFriends
	}

	conv, err := s.repo.FindConversation(ctx, senderID, peerID)
	if errors.Is(err, ErrConversationNotFound) {
		conv, err = s.repo.EnsureConversation(ctx, senderID, peerID)
	}
	if err != nil {
		return nil, err
	}

	m := &Message{
		ConversationID: conv.ID,
		SenderID:       senderID,
		ClientMsgID:    clientMsgID,
		Type:           TypeText,
		Content:        content,
	}
	err = s.repo.Create(ctx, m)
	if errors.Is(err, ErrDuplicateMessage) {
		// 同一条消息的两次提交并发到达（例如两次重发挤在一起）：两边都没查到，只有一边插入成功。
		// 失败的这边回滚了事务，再查一次就能拿到成功的那条。
		existing, err := s.findExisting(ctx, senderID, clientMsgID)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			return nil, errors.New("send message: duplicate reported but message not found")
		}
		return existing, nil
	}
	if err != nil {
		return nil, err
	}
	m.RecipientID = peerID

	slog.InfoContext(ctx, "message stored",
		"conversation_id", m.ConversationID,
		"message_id", m.ID,
		"seq", m.Seq,
		"client_msg_id", m.ClientMsgID,
		"to_user_id", peerID,
		"content_len", utf8.RuneCountInString(content), // 不记录消息正文，只记录长度
	)

	// 推送给接收方，也推送给发送方自己：发送方在其他标签页/设备上的连接也要看到这条消息（FR-04 多处登录同步）。
	// 发送这条消息的那个连接也会收到，客户端按 client_msg_id 合并，不会显示两条。
	s.notifier.MessageCreated(ctx, m, []int64{peerID, senderID})
	return &SendResult{Message: m}, nil
}

// findExisting 按 client_msg_id 查已写入的消息。查不到返回 (nil, nil)。
func (s *Service) findExisting(ctx context.Context, senderID int64, clientMsgID string) (*SendResult, error) {
	m, err := s.repo.FindByClientMsgID(ctx, senderID, clientMsgID)
	if errors.Is(err, ErrMessageNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	conv, err := s.repo.FindConversationByID(ctx, m.ConversationID)
	if err != nil {
		return nil, err
	}
	m.RecipientID = conv.peerOf(senderID)
	// Debug 级别：重发本身是正常现象（网络抖动、ACK 丢失），不需要在 Info 里刷屏
	slog.DebugContext(ctx, "duplicate message ignored", "client_msg_id", clientMsgID, "message_id", m.ID, "seq", m.Seq)
	return &SendResult{Message: m, Duplicate: true}, nil
}

// Page 一页历史消息，按 seq 升序
type Page struct {
	Messages []Message
	HasMore  bool // 同方向上还有更多消息（ListAfter 是更新的，ListBefore 是更早的）
}

// ListAfter me 与 peerID 的会话中 seq > afterSeq 的消息（FR-17 增量同步：打开会话、断线重连后补齐）。
// 只能查自己参与的会话（NFR-03）：会话按 (me, peerID) 查找，天然不会查到别人的会话。
func (s *Service) ListAfter(ctx context.Context, me, peerID, afterSeq int64, limit int) (*Page, error) {
	if afterSeq < 0 {
		afterSeq = 0
	}
	return s.list(ctx, me, peerID, limit, false, func(convID int64, n int) ([]Message, error) {
		return s.repo.ListAfter(ctx, convID, afterSeq, n)
	})
}

// ListBefore me 与 peerID 的会话中 seq < beforeSeq 的最近一页消息；beforeSeq <= 0 表示最新一页（FR-18）。
func (s *Service) ListBefore(ctx context.Context, me, peerID, beforeSeq int64, limit int) (*Page, error) {
	return s.list(ctx, me, peerID, limit, true, func(convID int64, n int) ([]Message, error) {
		return s.repo.ListBefore(ctx, convID, beforeSeq, n)
	})
}

// list ListAfter 和 ListBefore 的公共流程。
// backward 表示向前（更早）翻页：query 返回的是最近的 limit+1 条，多出来的一条在最前面；
// 否则 query 返回的是最早的 limit+1 条，多出来的一条在最后面。
func (s *Service) list(ctx context.Context, me, peerID int64, limit int, backward bool,
	query func(convID int64, n int) ([]Message, error)) (*Page, error) {
	if peerID <= 0 || peerID == me {
		return nil, ErrInvalidPeer
	}
	if limit <= 0 || limit > MaxPageSize {
		limit = DefaultPageSize
	}

	conv, err := s.repo.FindConversation(ctx, me, peerID)
	if errors.Is(err, ErrConversationNotFound) {
		return &Page{}, nil // 还没聊过，没有消息
	}
	if err != nil {
		return nil, err
	}

	// 多查一条判断还有没有下一页，比再 COUNT 一次便宜
	msgs, err := query(conv.ID, limit+1)
	if err != nil {
		return nil, err
	}
	page := &Page{HasMore: len(msgs) > limit}
	if page.HasMore {
		if backward {
			msgs = msgs[1:]
		} else {
			msgs = msgs[:limit]
		}
	}
	for i := range msgs {
		msgs[i].RecipientID = conv.peerOf(msgs[i].SenderID)
	}
	page.Messages = msgs
	return page, nil
}

func validateClientMsgID(id string) error {
	if id == "" || len(id) > maxClientMsgIDLen {
		return ErrInvalidClientMsgID
	}
	// 只允许字母、数字、- 和 _（UUID 的字符集是它的子集），避免奇怪的字符进入日志和数据库
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return ErrInvalidClientMsgID
		}
	}
	return nil
}

// validateContent 内容原样保存（保留换行和首尾空格），但不能全是空白，长度按字符数（rune）计算
func validateContent(content string) error {
	if strings.TrimSpace(content) == "" {
		return ErrEmptyContent
	}
	if utf8.RuneCountInString(content) > MaxContentLen {
		return ErrContentTooLong
	}
	// PostgreSQL 的 text 不能存 NUL 字符（\x00），直接写库会报错变成 500，这里提前拒绝
	if strings.ContainsRune(content, 0) {
		return ErrInvalidContent
	}
	return nil
}
