package message

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"im-server/internal/user"
)

// fakeRepo 用内存模拟 conversations / conversation_members / messages 三张表，并模拟库里的约束和事务语义：
//   - uq_conversations_pair：一对用户只有一个会话；
//   - Create 在锁内分配 seq 并插入，(sender_id, client_msg_id) 冲突时返回 ErrDuplicateMessage 且不占用 seq（事务回滚）；
//     成功时把发送方的 read_seq 推进到新 seq。
type fakeRepo struct {
	mu       sync.Mutex
	nextConv int64
	nextMsg  int64
	convs    map[int64]*Conversation
	msgs     []Message
	reads    map[[2]int64]int64 // (conversation_id, user_id) → read_seq，即 conversation_members

	// beforeCreate 测试钩子：在 Create 分配 seq 之前调用，用来模拟"查重之后、插入之前"另一个请求抢先写入
	beforeCreate func(m *Message)
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{convs: map[int64]*Conversation{}, reads: map[[2]int64]int64{}}
}

func (r *fakeRepo) findPair(a, b int64) *Conversation {
	ua, ub := pairOf(a, b)
	for _, c := range r.convs {
		if c.UserA == ua && c.UserB == ub {
			return c
		}
	}
	return nil
}

func (r *fakeRepo) FindConversation(_ context.Context, a, b int64) (*Conversation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.findPair(a, b); c != nil {
		cp := *c
		return &cp, nil
	}
	return nil, ErrConversationNotFound
}

func (r *fakeRepo) FindConversationByID(_ context.Context, id int64) (*Conversation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.convs[id]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, ErrConversationNotFound
}

func (r *fakeRepo) EnsureConversation(_ context.Context, a, b int64) (*Conversation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.findPair(a, b)
	if c == nil {
		r.nextConv++
		ua, ub := pairOf(a, b)
		c = &Conversation{ID: r.nextConv, UserA: ua, UserB: ub, CreatedAt: time.Now()}
		r.convs[c.ID] = c
		r.reads[[2]int64{c.ID, ua}] = 0
		r.reads[[2]int64{c.ID, ub}] = 0
	}
	cp := *c
	return &cp, nil
}

func (r *fakeRepo) FindByClientMsgID(_ context.Context, senderID int64, clientMsgID string) (*Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.msgs {
		if m.SenderID == senderID && m.ClientMsgID == clientMsgID {
			cp := m
			return &cp, nil
		}
	}
	return nil, ErrMessageNotFound
}

func (r *fakeRepo) Create(_ context.Context, m *Message) error {
	if r.beforeCreate != nil {
		r.beforeCreate(m)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.msgs {
		if existing.SenderID == m.SenderID && existing.ClientMsgID == m.ClientMsgID {
			return ErrDuplicateMessage // 事务回滚：last_seq 不变
		}
	}
	c, ok := r.convs[m.ConversationID]
	if !ok {
		return ErrConversationNotFound
	}
	c.LastSeq++
	now := time.Now()
	c.LastMessageAt = &now
	r.nextMsg++
	m.ID = r.nextMsg
	m.Seq = c.LastSeq
	m.CreatedAt = now
	r.msgs = append(r.msgs, *m)
	key := [2]int64{c.ID, m.SenderID}
	r.reads[key] = max(r.reads[key], m.Seq)
	return nil
}

func (r *fakeRepo) ListConversations(_ context.Context, userID int64) ([]ConversationSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ConversationSummary
	for _, c := range r.convs {
		if c.LastSeq == 0 || (c.UserA != userID && c.UserB != userID) {
			continue
		}
		sum := ConversationSummary{Conversation: *c, ReadSeq: r.reads[[2]int64{c.ID, userID}]}
		for _, m := range r.msgs {
			if m.ConversationID == c.ID && m.Seq == c.LastSeq {
				sum.LastMessage = m
			}
		}
		out = append(out, sum)
	}
	// 和 SQL 一致：last_message_at DESC, id DESC
	slices.SortFunc(out, func(a, b ConversationSummary) int {
		return cmp.Or(
			b.Conversation.LastMessageAt.Compare(*a.Conversation.LastMessageAt),
			cmp.Compare(b.Conversation.ID, a.Conversation.ID),
		)
	})
	return out, nil
}

func (r *fakeRepo) MarkRead(_ context.Context, convID, userID, seq int64) (int64, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := [2]int64{convID, userID}
	cur, ok := r.reads[key]
	if !ok {
		return 0, false, ErrConversationNotFound
	}
	target := min(seq, r.convs[convID].LastSeq)
	if cur >= target {
		return cur, false, nil
	}
	r.reads[key] = target
	return target, true, nil
}

func (r *fakeRepo) readSeq(convID, userID int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads[[2]int64{convID, userID}]
}

func (r *fakeRepo) ListAfter(_ context.Context, convID, afterSeq int64, limit int) ([]Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Message
	for _, m := range r.msgs { // msgs 按插入顺序即 seq 升序
		if m.ConversationID == convID && m.Seq > afterSeq && len(out) < limit {
			out = append(out, m)
		}
	}
	return out, nil
}

func (r *fakeRepo) ListBefore(_ context.Context, convID, beforeSeq int64, limit int) ([]Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []Message
	for _, m := range r.msgs {
		if m.ConversationID == convID && (beforeSeq <= 0 || m.Seq < beforeSeq) {
			all = append(all, m)
		}
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

// fakeFriends 好友关系，key 为排好序的一对用户
type fakeFriends map[[2]int64]bool

func (f fakeFriends) AreFriends(_ context.Context, a, b int64) (bool, error) {
	ua, ub := pairOf(a, b)
	return f[[2]int64{ua, ub}], nil
}

func (f fakeFriends) set(a, b int64, ok bool) {
	ua, ub := pairOf(a, b)
	f[[2]int64{ua, ub}] = ok
}

// fakeDirectory 用户资料，不存在的 ID 直接跳过（和 *user.Service 的约定一致）
type fakeDirectory map[int64]user.Profile

func (d fakeDirectory) GetProfiles(_ context.Context, ids []int64) ([]user.Profile, error) {
	var out []user.Profile
	for _, id := range ids {
		if p, ok := d[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// fakeNotifier 记录每次推送：哪条消息推给了哪些人、谁的已读位置推进到了哪里
type fakeNotifier struct {
	mu     sync.Mutex
	events []pushed
	reads  []readPushed
}

type readPushed struct {
	userID, peerID, readSeq int64
}

func (n *fakeNotifier) ReadUpdated(_ context.Context, userID, peerID, readSeq int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reads = append(n.reads, readPushed{userID, peerID, readSeq})
}

func (n *fakeNotifier) readEvents() []readPushed {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.reads)
}

type pushed struct {
	seq        int64
	recipients []int64
}

func (n *fakeNotifier) MessageCreated(_ context.Context, m *Message, recipients []int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, pushed{seq: m.Seq, recipients: recipients})
}

func (n *fakeNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.events)
}
