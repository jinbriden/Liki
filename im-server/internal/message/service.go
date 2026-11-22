package message

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"im-server/internal/user"
)

const (
	// MaxContentLen 单条消息最多 1500 个字符（按 rune 计数，FR-13），和库里的 CHECK 约束一致
	MaxContentLen = 1500
	// maxClientMsgIDLen client_msg_id 最长 64 个字符，和库里的 VARCHAR(64) 一致。前端用 UUID（36 个字符）
	maxClientMsgIDLen = 64

	DefaultPageSize = 50
	MaxPageSize     = 100

	// MaxDeleteBatch 一次最多删除 30 条消息（FR-19）
	MaxDeleteBatch = 30
	// 删除记录同步每页条数
	DefaultDeletionPage = 200
	MaxDeletionPage     = 500
)

// FriendChecker 消息模块需要的好友关系查询能力，由 *friend.Service 实现。
// 好友之间发消息不受限；非好友之间按 FR-12 限制为"对方回复前只能发一条"。
type FriendChecker interface {
	AreFriends(ctx context.Context, a, b int64) (bool, error)
}

// UserDirectory 消息模块需要的用户资料查询能力（会话列表显示对方头像和用户名），由 *user.Service 实现。
// 和好友模块一样不直接查 users 表、不跨模块 JOIN（NFR-07）。
type UserDirectory interface {
	// GetProfiles 批量查用户资料；不存在的 ID 直接跳过，返回顺序不保证。
	GetProfiles(ctx context.Context, ids []int64) ([]user.Profile, error)
}

// Notifier 消息相关的实时通知。
// service 只决定"通知谁"，推送的数据格式和怎么送达由实现决定（见 notify.go），service 不依赖 WebSocket。
// 推送是尽力而为的，不返回错误：没收到也没关系，重连后会按 seq 增量同步、刷新会话列表补齐。
type Notifier interface {
	// MessageCreated 新消息写库成功，推送给 recipients 的所有连接
	MessageCreated(ctx context.Context, m *Message, recipients []int64)
	// ReadUpdated userID 在与 peerID 的会话中的已读位置推进到了 readSeq，
	// 同步给他自己的所有连接（FR-04、FR-15：多处登录时未读数一致）。不通知对方：本阶段不做已读回执。
	ReadUpdated(ctx context.Context, userID, peerID, readSeq int64)
	// MessagesDeleted userID 删除了与 peerID 的会话中的这些消息，同步给他自己的所有连接（FR-19）。不通知对方
	MessagesDeleted(ctx context.Context, userID, peerID int64, seqs []int64)
	// ChatCleared userID 清空了与 peerID 的聊天记录，同步给他自己的所有连接（FR-19）。不通知对方
	ChatCleared(ctx context.Context, userID, peerID, clearedSeq, readSeq int64)
}

// Limiter 限流器，由 ratelimit 包的实现满足
type Limiter interface {
	Allow(ctx context.Context, key string) bool
}

type Service struct {
	repo     Repository
	friends  FriendChecker
	users    UserDirectory
	notifier Notifier
	// sendLimit 按发送方限制发消息的频率（NFR-05）。nil 表示不限制（单元测试默认如此）
	sendLimit Limiter
}

// Option 可选配置（函数式选项），不需要的调用方不用传
type Option func(*Service)

// WithSendLimiter 按发送方用户 ID 限制发消息的频率
func WithSendLimiter(l Limiter) Option {
	return func(s *Service) { s.sendLimit = l }
}

func NewService(repo Repository, friends FriendChecker, users UserDirectory, notifier Notifier, opts ...Option) *Service {
	s := &Service{repo: repo, friends: friends, users: users, notifier: notifier}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// SendResult 发送结果。Duplicate 为 true 表示这条消息之前已经写过（客户端重发），返回的是已有的那条。
type SendResult struct {
	Message   *Message
	Duplicate bool
}

// Send senderID 给 peerID 发一条文字消息（FR-10、FR-11、FR-13）。
//
// 流程：校验 → 按 client_msg_id 去重 → 限流 → 查好友关系 → 找到（或创建）会话 → 事务内分配 seq、检查非好友限制并写库 → 推送。
// 重复提交（同一个 sender_id + client_msg_id）时不重复写库，直接返回已有消息和它的 seq，客户端据此确认。
//
// 非好友限制（FR-12）：会话里上一条消息也是自己发的，就不能再发（ErrStrangerLimit）；对方回复一条后可以再发一条。
// 这个判断不能在 service 里先查再写：同一个人在两个标签页同时发，两边都会查到"上一条是对方的"而都通过。
// 所以 service 只负责查好友关系（属于好友模块，放不进消息的事务），"上一条是谁发的"交给 repository 在持有会话行锁时判断。
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

	// 限流放在去重之后：客户端没收到 ACK 时的重发不算新消息，不消耗配额（NFR-05）。
	// 否则网络抖动时的自动重发会把用户的配额用光，新消息反而被拒绝
	if s.sendLimit != nil && !s.sendLimit.Allow(ctx, strconv.FormatInt(senderID, 10)) {
		return nil, ErrRateLimited
	}

	friends, err := s.friends.AreFriends(ctx, senderID, peerID)
	if err != nil {
		return nil, err
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
	err = s.repo.Create(ctx, m, !friends)
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
		"friends", friends,
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
	// LastSeq 查询这一页之前会话的最新 seq。消息被删除后，客户端看到的最后一条不一定是 last_seq，
	// 没有更多时客户端据此把"已同步区间"补到 LastSeq，否则会以为后面还有消息、反复去拉（FR-17）
	LastSeq int64
}

// ListAfter me 与 peerID 的会话中 seq > afterSeq 的消息（FR-17 增量同步：打开会话、断线重连后补齐）。
// 只能查自己参与的会话（NFR-03）：会话按 (me, peerID) 查找，天然不会查到别人的会话。
// 只返回 me 能看到的消息：自己删除、清空掉的不返回（FR-20），下同。
func (s *Service) ListAfter(ctx context.Context, me, peerID, afterSeq int64, limit int) (*Page, error) {
	if afterSeq < 0 {
		afterSeq = 0
	}
	return s.list(ctx, me, peerID, limit, false, func(convID int64, n int) ([]Message, error) {
		return s.repo.ListAfter(ctx, convID, me, afterSeq, n)
	})
}

// ListBefore me 与 peerID 的会话中 seq < beforeSeq 的最近一页消息；beforeSeq <= 0 表示最新一页（FR-18）。
func (s *Service) ListBefore(ctx context.Context, me, peerID, beforeSeq int64, limit int) (*Page, error) {
	return s.list(ctx, me, peerID, limit, true, func(convID int64, n int) ([]Message, error) {
		return s.repo.ListBefore(ctx, convID, me, beforeSeq, n)
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
	// conv 是在查消息之前读的，LastSeq 不会比这一页里的消息新：这期间新到的消息要么不在这一页里，
	// 要么 seq 比 LastSeq 大（客户端取两者较大值），不会把没拉到的消息误记成"已同步"
	page := &Page{HasMore: len(msgs) > limit, LastSeq: conv.LastSeq}
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

// ConversationView 会话列表中的一项（FR-14）
type ConversationView struct {
	Peer          user.Profile
	LastMessage   *Message // 我能看到的最后一条；全部删除或清空后为 nil
	LastSeq       int64
	ReadSeq       int64
	ClearedSeq    int64 // 清空位置，客户端据此删除本地缓存里更早的消息（多设备同步清空，FR-20）
	Unread        int64 // = LastSeq - ReadSeq（FR-15）。自己发的消息不会被算进来：发送时 read_seq 已推进到它的 seq
	LastMessageAt time.Time
}

// ListConversations me 参与的、有消息的会话，按最后一条消息的时间倒序（FR-14）。不分页，第一阶段会话数量有限。
func (s *Service) ListConversations(ctx context.Context, me int64) ([]ConversationView, error) {
	sums, err := s.repo.ListConversations(ctx, me)
	if err != nil {
		return nil, err
	}
	if len(sums) == 0 {
		return []ConversationView{}, nil
	}

	peerIDs := make([]int64, 0, len(sums))
	for i := range sums {
		peerIDs = append(peerIDs, sums[i].Conversation.peerOf(me))
	}
	list, err := s.users.GetProfiles(ctx, peerIDs)
	if err != nil {
		return nil, err
	}
	profiles := make(map[int64]user.Profile, len(list))
	for _, p := range list {
		profiles[p.ID] = p
	}

	views := make([]ConversationView, 0, len(sums))
	for i := range sums {
		sum := &sums[i]
		conv := &sum.Conversation
		peerID := conv.peerOf(me)
		peer, ok := profiles[peerID]
		if !ok {
			peer = user.Profile{ID: peerID} // 理论上外键保证查得到；查不到时只返回 ID，不让整个列表失败
		}
		if last := sum.LastMessage; last != nil {
			last.RecipientID = conv.peerOf(last.SenderID)
		}
		views = append(views, ConversationView{
			Peer:          peer,
			LastMessage:   sum.LastMessage,
			LastSeq:       conv.LastSeq,
			ReadSeq:       sum.ReadSeq,
			ClearedSeq:    sum.ClearedSeq,
			Unread:        max(0, conv.LastSeq-sum.ReadSeq),
			LastMessageAt: *conv.LastMessageAt,
		})
	}
	return views, nil
}

// MarkRead 把 me 在与 peerID 的会话中的已读位置推进到 seq（FR-15），返回推进后的 read_seq。
//
// "什么时候算看到"由客户端判断（消息真正出现在可视范围内、点了"标记为已读"），服务端只保证：
// read_seq 只增不减、不超过 last_seq。所以重复上报、乱序上报、多个标签页同时上报都是安全的。
// 还没聊过（会话不存在）时返回 0，不报错。read_seq 真的变了才推送给自己的其他连接。
func (s *Service) MarkRead(ctx context.Context, me, peerID, seq int64) (int64, error) {
	if peerID <= 0 || peerID == me {
		return 0, ErrInvalidPeer
	}
	if seq < 0 {
		return 0, ErrInvalidReadSeq
	}

	conv, err := s.repo.FindConversation(ctx, me, peerID)
	if errors.Is(err, ErrConversationNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	readSeq, changed, err := s.repo.MarkRead(ctx, conv.ID, me, seq)
	if errors.Is(err, ErrConversationNotFound) {
		return 0, nil // 会话存在但我不是成员：单聊里不会发生，按"没聊过"处理
	}
	if err != nil {
		return 0, err
	}
	if changed {
		// Debug：阅读时会频繁上报，属于"正常但频繁"的情况
		slog.DebugContext(ctx, "read seq advanced", "conversation_id", conv.ID, "peer_id", peerID, "read_seq", readSeq)
		s.notifier.ReadUpdated(ctx, me, peerID, readSeq)
	}
	return readSeq, nil
}

// DeleteMessages me 删除与 peerID 的会话中的若干条消息（FR-19）：只对自己生效，对方仍能看到，服务端不物理删除（FR-21）。
// 一次 1～30 条；seq 必须是会话里已有的（1..last_seq），重复的 seq 只算一次。已经删过的再删一次算成功（幂等）。
// 有新删除的才推送给自己的所有连接（包括发起请求的标签页，HTTP 请求不知道来自哪个连接；客户端重复删除没有影响）。
func (s *Service) DeleteMessages(ctx context.Context, me, peerID int64, seqs []int64) error {
	if peerID <= 0 || peerID == me {
		return ErrInvalidPeer
	}
	seqs = slices.Clone(seqs) // 不修改调用方的切片
	slices.Sort(seqs)
	seqs = slices.Compact(seqs) // 排序后去掉相邻的重复值，类似 Java 的 stream().distinct()
	if len(seqs) == 0 || len(seqs) > MaxDeleteBatch {
		return ErrInvalidDeleteCount
	}

	conv, err := s.repo.FindConversation(ctx, me, peerID)
	if errors.Is(err, ErrConversationNotFound) {
		return ErrInvalidSeq // 没聊过，自然没有可删的消息
	}
	if err != nil {
		return err
	}
	// 排过序，只要看两端：消息永不物理删除、seq 连续分配，1..last_seq 之间的每个 seq 都有消息
	if seqs[0] < 1 || seqs[len(seqs)-1] > conv.LastSeq {
		return ErrInvalidSeq
	}

	deleted, err := s.repo.DeleteMessages(ctx, conv.ID, me, seqs)
	if err != nil {
		return err
	}
	if len(deleted) == 0 {
		slog.DebugContext(ctx, "messages already deleted", "conversation_id", conv.ID, "count", len(seqs))
		return nil
	}
	slog.InfoContext(ctx, "messages deleted", "conversation_id", conv.ID, "peer_id", peerID, "count", len(deleted))
	s.notifier.MessagesDeleted(ctx, me, peerID, deleted)
	return nil
}

// ClearResult 清空之后的位置
type ClearResult struct {
	ClearedSeq int64
	ReadSeq    int64
}

// Clear me 清空与 peerID 的聊天记录（FR-19）：seq <= upToSeq 的消息对自己不再可见，同时视为已读。
// upToSeq 是用户点"清空"时客户端已知的最新 seq：之后才到的消息不受影响，避免把还没看到的新消息一起清掉。
// 超过会话最新 seq 时按最新 seq 算；清空位置只增不减。还没聊过时返回全 0。清空位置真的变了才推送。
func (s *Service) Clear(ctx context.Context, me, peerID, upToSeq int64) (*ClearResult, error) {
	if peerID <= 0 || peerID == me {
		return nil, ErrInvalidPeer
	}
	if upToSeq < 0 {
		return nil, ErrInvalidClearSeq
	}

	conv, err := s.repo.FindConversation(ctx, me, peerID)
	if errors.Is(err, ErrConversationNotFound) {
		return &ClearResult{}, nil
	}
	if err != nil {
		return nil, err
	}

	cleared, read, changed, err := s.repo.Clear(ctx, conv.ID, me, upToSeq)
	if errors.Is(err, ErrConversationNotFound) {
		return &ClearResult{}, nil // 会话存在但我不是成员：单聊里不会发生，按"没聊过"处理
	}
	if err != nil {
		return nil, err
	}
	if changed {
		slog.InfoContext(ctx, "chat cleared", "conversation_id", conv.ID, "peer_id", peerID, "cleared_seq", cleared)
		s.notifier.ChatCleared(ctx, me, peerID, cleared, read)
	}
	return &ClearResult{ClearedSeq: cleared, ReadSeq: read}, nil
}

// ListDeletions me 的删除记录中 id > afterID 的一页（FR-20）：客户端登录、重连时按游标拉取，
// 删掉本地缓存里离线期间在其他设备上删除的消息。返回的 HasMore 为 true 时以最后一条的 ID 继续拉。
func (s *Service) ListDeletions(ctx context.Context, me, afterID int64, limit int) ([]Deletion, bool, error) {
	if afterID < 0 {
		afterID = 0
	}
	if limit <= 0 || limit > MaxDeletionPage {
		limit = DefaultDeletionPage
	}
	items, err := s.repo.ListDeletions(ctx, me, afterID, limit+1)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	return items, hasMore, nil
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
