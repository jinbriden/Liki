package message

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Repository 消息模块的数据访问。数据库错误在这里翻译成业务哨兵错误，service 不 import gorm。
type Repository interface {
	// FindConversation 按一对用户查会话（参数顺序无所谓），没有时返回 ErrConversationNotFound。
	FindConversation(ctx context.Context, a, b int64) (*Conversation, error)

	// FindConversationByID 按 ID 查会话，没有时返回 ErrConversationNotFound。
	FindConversationByID(ctx context.Context, id int64) (*Conversation, error)

	// EnsureConversation 查一对用户的会话，没有就创建（连同两条 conversation_members）。
	// 两人同时给对方发第一条消息时只会建出一个会话，由唯一约束 uq_conversations_pair 兜底。
	EnsureConversation(ctx context.Context, a, b int64) (*Conversation, error)

	// FindByClientMsgID 按 (sender_id, client_msg_id) 查消息，没有时返回 ErrMessageNotFound。
	FindByClientMsgID(ctx context.Context, senderID int64, clientMsgID string) (*Message, error)

	// Create 在一个事务里给会话分配下一个 seq、插入消息，并把发送方的 read_seq 推进到这个 seq（FR-15 发送即已读）；
	// 成功后 m.ID、m.Seq、m.CreatedAt 由数据库回填。
	// (sender_id, client_msg_id) 已存在 → ErrDuplicateMessage，此时整个事务回滚，seq 不会被占用。
	Create(ctx context.Context, m *Message) error

	// ListConversations userID 参与的、至少有一条消息的会话，按最后一条消息的时间倒序（FR-14）。
	ListConversations(ctx context.Context, userID int64) ([]ConversationSummary, error)

	// MarkRead 把 userID 在会话中的 read_seq 推进到 seq（FR-15）：只增不减，超过 last_seq 时按 last_seq 算。
	// 返回推进后的 read_seq，changed 表示这次是否真的变了。userID 不是会话成员时返回 ErrConversationNotFound。
	MarkRead(ctx context.Context, conversationID, userID, seq int64) (readSeq int64, changed bool, err error)

	// ListAfter 会话中 seq > afterSeq 的消息，按 seq 升序，最多 limit 条（增量同步）。
	ListAfter(ctx context.Context, conversationID, afterSeq int64, limit int) ([]Message, error)

	// ListBefore 会话中 seq < beforeSeq 的最近 limit 条消息，按 seq 升序返回（向前翻页）。
	// beforeSeq <= 0 表示从最新一条开始取。
	ListBefore(ctx context.Context, conversationID, beforeSeq int64, limit int) ([]Message, error)
}

type gormRepository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) Repository {
	return &gormRepository{db: db}
}

func (r *gormRepository) FindConversation(ctx context.Context, a, b int64) (*Conversation, error) {
	return findConversation(r.db.WithContext(ctx), a, b)
}

func (r *gormRepository) FindConversationByID(ctx context.Context, id int64) (*Conversation, error) {
	var c Conversation
	err := r.db.WithContext(ctx).Take(&c, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func findConversation(db *gorm.DB, a, b int64) (*Conversation, error) {
	userA, userB := pairOf(a, b)
	var c Conversation
	err := db.Where("user_a = ? AND user_b = ?", userA, userB).Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *gormRepository) EnsureConversation(ctx context.Context, a, b int64) (*Conversation, error) {
	userA, userB := pairOf(a, b)
	var conv *Conversation
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// ON CONFLICT DO NOTHING：会话已存在（或并发下被别人先建了）时不报错，接着查出来即可。
		// 不用"先查再插"：两人同时发第一条消息时，两边都查不到、都去插，会撞唯一约束。
		c := Conversation{UserA: userA, UserB: userB}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&c).Error; err != nil {
			return err
		}
		found, err := findConversation(tx, userA, userB)
		if err != nil {
			return err
		}
		members := []ConversationMember{
			{ConversationID: found.ID, UserID: userA},
			{ConversationID: found.ID, UserID: userB},
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&members).Error; err != nil {
			return err
		}
		conv = found
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ensure conversation: %w", err)
	}
	return conv, nil
}

func (r *gormRepository) FindByClientMsgID(ctx context.Context, senderID int64, clientMsgID string) (*Message, error) {
	var m Message
	err := r.db.WithContext(ctx).
		Where("sender_id = ? AND client_msg_id = ?", senderID, clientMsgID).
		Take(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrMessageNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *gormRepository) Create(ctx context.Context, m *Message) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1. 分配 seq：UPDATE 会给这一行会话加行锁，同一会话的并发发送在这里排队，
		//    拿到的 seq 一定各不相同、连续递增。锁一直持有到事务提交或回滚。
		//    last_message_at 和下面消息的 created_at 都用 now()，同一事务内 now() 是同一个值，两者一致。
		var seq int64
		res := tx.Raw(
			"UPDATE conversations SET last_seq = last_seq + 1, last_message_at = now() WHERE id = ? RETURNING last_seq",
			m.ConversationID,
		).Scan(&seq)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrConversationNotFound
		}
		m.Seq = seq

		// 2. 插入消息。(sender_id, client_msg_id) 冲突说明这条消息已经写过（客户端重发、并发重发），
		//    返回错误让事务回滚，上面 +1 的 seq 也一起撤销，不会在会话里留下空洞。
		//    (conversation_id, seq) 有行锁保护，不会冲突，所以 ErrDuplicatedKey 只可能来自去重约束。
		err := tx.Create(m).Error
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return ErrDuplicateMessage
		}
		if err != nil {
			return err
		}

		// 3. 发送即已读（FR-15）：发送方回复了，视为已读完这条消息之前的所有消息。
		//    和分配 seq 放在同一个事务里，要么一起生效、要么一起回滚。
		//    GREATEST 只是保险：seq 是刚分配的最大值，正常情况下一定不小于原来的 read_seq。
		return tx.Exec(
			"UPDATE conversation_members SET read_seq = GREATEST(read_seq, ?) WHERE conversation_id = ? AND user_id = ?",
			seq, m.ConversationID, m.SenderID,
		).Error
	})
}

// conversationRow ListConversations 查询结果的一行（会话 + 我的成员记录 + 最后一条消息拍平在一起）。
// GORM 按字段名的 snake_case 映射列名，所以 SQL 里用 AS 把列名对上。
type conversationRow struct {
	ConversationID int64
	UserA          int64
	UserB          int64
	LastSeq        int64
	LastMessageAt  time.Time
	ConvCreatedAt  time.Time
	ReadSeq        int64
	MessageID      int64
	SenderID       int64
	ClientMsgID    string
	Type           string
	Content        string
	CreatedAt      time.Time
}

func (r *gormRepository) ListConversations(ctx context.Context, userID int64) ([]ConversationSummary, error) {
	// 最后一条消息按 (conversation_id, seq = last_seq) 取，走唯一索引 uq_messages_conversation_seq。
	// last_seq > 0 过滤掉"建了会话但还没有消息"的情况（此时 last_message_at 为 NULL）。
	// M6 加入逐条删除后，"最后一条"要排除自己删掉的消息，这里会改成 LATERAL 子查询。
	var rows []conversationRow
	err := r.db.WithContext(ctx).Raw(`
SELECT c.id AS conversation_id, c.user_a, c.user_b, c.last_seq, c.last_message_at, c.created_at AS conv_created_at,
       cm.read_seq,
       m.id AS message_id, m.sender_id, m.client_msg_id, m.type, m.content, m.created_at
FROM conversation_members cm
JOIN conversations c ON c.id = cm.conversation_id
JOIN messages m ON m.conversation_id = c.id AND m.seq = c.last_seq
WHERE cm.user_id = ? AND c.last_seq > 0
ORDER BY c.last_message_at DESC, c.id DESC`, userID).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	out := make([]ConversationSummary, 0, len(rows))
	for _, row := range rows {
		lastAt := row.LastMessageAt
		out = append(out, ConversationSummary{
			Conversation: Conversation{
				ID:            row.ConversationID,
				UserA:         row.UserA,
				UserB:         row.UserB,
				LastSeq:       row.LastSeq,
				LastMessageAt: &lastAt,
				CreatedAt:     row.ConvCreatedAt,
			},
			ReadSeq: row.ReadSeq,
			LastMessage: Message{
				ID:             row.MessageID,
				ConversationID: row.ConversationID,
				Seq:            row.LastSeq,
				SenderID:       row.SenderID,
				ClientMsgID:    row.ClientMsgID,
				Type:           row.Type,
				Content:        row.Content,
				CreatedAt:      row.CreatedAt,
			},
		})
	}
	return out, nil
}

func (r *gormRepository) MarkRead(ctx context.Context, conversationID, userID, seq int64) (int64, bool, error) {
	db := r.db.WithContext(ctx)

	// 只在"确实会变大"时才更新：重复上报同一个位置（多个标签页、节流边界）不产生写入，也不触发推送。
	// LEAST(?, c.last_seq)：客户端报的 seq 超过会话最大值时按最大值算，read_seq 不会跑到 last_seq 前面。
	// UPDATE ... FROM 只锁 conversation_members 这一行，不锁 conversations，不会和发消息的事务互相等待成环。
	var readSeq int64
	res := db.Raw(`
UPDATE conversation_members cm
SET read_seq = LEAST(?, c.last_seq)
FROM conversations c
WHERE c.id = cm.conversation_id AND cm.conversation_id = ? AND cm.user_id = ?
  AND cm.read_seq < LEAST(?, c.last_seq)
RETURNING cm.read_seq`, seq, conversationID, userID, seq).Scan(&readSeq)
	if res.Error != nil {
		return 0, false, res.Error
	}
	if res.RowsAffected == 1 {
		return readSeq, true, nil
	}

	// 没更新：要么已读位置本来就不小于 seq，要么根本不是成员。查一次当前值区分两者
	var m ConversationMember
	err := db.Where("conversation_id = ? AND user_id = ?", conversationID, userID).Take(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, ErrConversationNotFound
	}
	if err != nil {
		return 0, false, err
	}
	return m.ReadSeq, false, nil
}

func (r *gormRepository) ListAfter(ctx context.Context, conversationID, afterSeq int64, limit int) ([]Message, error) {
	var msgs []Message
	// 走唯一索引 uq_messages_conversation_seq 做范围扫描
	err := r.db.WithContext(ctx).
		Where("conversation_id = ? AND seq > ?", conversationID, afterSeq).
		Order("seq ASC").
		Limit(limit).
		Find(&msgs).Error
	return msgs, err
}

func (r *gormRepository) ListBefore(ctx context.Context, conversationID, beforeSeq int64, limit int) ([]Message, error) {
	q := r.db.WithContext(ctx).Where("conversation_id = ?", conversationID)
	if beforeSeq > 0 {
		q = q.Where("seq < ?", beforeSeq)
	}
	var msgs []Message
	// 取"最近的 limit 条"要倒序查，再在内存里翻转成升序，调用方统一按 seq 升序处理
	if err := q.Order("seq DESC").Limit(limit).Find(&msgs).Error; err != nil {
		return nil, err
	}
	slices.Reverse(msgs)
	return msgs, nil
}
