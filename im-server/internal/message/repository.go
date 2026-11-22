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
	// requireReply 为 true 表示双方不是好友（FR-12）：会话里上一条消息也是 m.SenderID 发的 → ErrStrangerLimit，同样整个事务回滚。
	// 两个错误同时满足时（非好友的重发）返回 ErrDuplicateMessage，让 service 按去重处理。
	Create(ctx context.Context, m *Message, requireReply bool) error

	// ListConversations userID 参与的、至少有一条消息的会话，按最后一条消息的时间倒序（FR-14）。
	ListConversations(ctx context.Context, userID int64) ([]ConversationSummary, error)

	// MarkRead 把 userID 在会话中的 read_seq 推进到 seq（FR-15）：只增不减，超过 last_seq 时按 last_seq 算。
	// 返回推进后的 read_seq，changed 表示这次是否真的变了。userID 不是会话成员时返回 ErrConversationNotFound。
	MarkRead(ctx context.Context, conversationID, userID, seq int64) (readSeq int64, changed bool, err error)

	// ListAfter 会话中 userID 能看到的、seq > afterSeq 的消息，按 seq 升序，最多 limit 条（增量同步）。
	// "能看到"指不在他的清空位置之前、也没被他删除（FR-20），下同。
	ListAfter(ctx context.Context, conversationID, userID, afterSeq int64, limit int) ([]Message, error)

	// ListBefore 会话中 userID 能看到的、seq < beforeSeq 的最近 limit 条消息，按 seq 升序返回（向前翻页）。
	// beforeSeq <= 0 表示从最新一条开始取。
	ListBefore(ctx context.Context, conversationID, userID, beforeSeq int64, limit int) ([]Message, error)

	// DeleteMessages userID 删除会话中的若干条消息（只对自己生效，FR-19）。seqs 必须都是会话里已有的 seq。
	// 已经删过的跳过（幂等），返回这次新删除的 seq，按升序。
	DeleteMessages(ctx context.Context, conversationID, userID int64, seqs []int64) ([]int64, error)

	// Clear 把 userID 在会话中的清空位置推进到 upToSeq（超过 last_seq 时按 last_seq 算），只增不减；
	// 同时把 read_seq 推进到同一位置（清空视为已读，FR-19）。
	// 返回推进后的 cleared_seq、read_seq，changed 表示清空位置是否真的变了。userID 不是会话成员时返回 ErrConversationNotFound。
	Clear(ctx context.Context, conversationID, userID, upToSeq int64) (clearedSeq, readSeq int64, changed bool, err error)

	// ListDeletions userID 的删除记录中 id > afterID 的，按 id 升序，最多 limit 条（多设备增量同步，FR-20）。
	ListDeletions(ctx context.Context, userID, afterID int64, limit int) ([]Deletion, error)
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

func (r *gormRepository) Create(ctx context.Context, m *Message, requireReply bool) error {
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

		// 3. 非好友限制（FR-12）：上一条消息（seq - 1）也是自己发的，就拒绝。
		//    放在分配 seq 之后：此时持有会话行锁，同一会话的并发发送排队执行，后执行的一定能看到先提交的消息
		//    （READ COMMITTED 下每条语句取新快照），不会两条都通过。
		//    放在插入之后：非好友重发已写入的消息时，先撞上去重约束返回 ErrDuplicateMessage，
		//    而不是被误判成"上一条是自己发的"，否则客户端会把一条其实已发送成功的消息标记为失败。
		//    消息永不物理删除（FR-21），seq - 1 那条一定存在；用户自己删除的消息也照样计入。
		if requireReply && seq > 1 {
			var prevSender int64
			if err := tx.Raw(
				"SELECT sender_id FROM messages WHERE conversation_id = ? AND seq = ?",
				m.ConversationID, seq-1,
			).Scan(&prevSender).Error; err != nil {
				return err
			}
			if prevSender == m.SenderID {
				return ErrStrangerLimit
			}
		}

		// 4. 发送即已读（FR-15）：发送方回复了，视为已读完这条消息之前的所有消息。
		//    和分配 seq 放在同一个事务里，要么一起生效、要么一起回滚。
		//    GREATEST 只是保险：seq 是刚分配的最大值，正常情况下一定不小于原来的 read_seq。
		return tx.Exec(
			"UPDATE conversation_members SET read_seq = GREATEST(read_seq, ?) WHERE conversation_id = ? AND user_id = ?",
			seq, m.ConversationID, m.SenderID,
		).Error
	})
}

// conversationRow ListConversations 查询结果的一行（会话 + 我的成员记录 + 我能看到的最后一条消息拍平在一起）。
// GORM 按字段名的 snake_case 映射列名，所以 SQL 里用 AS 把列名对上。
// 消息的列来自 LEFT JOIN，全部删光时为 NULL，所以用指针（类似 Java 里用 Long 而不是 long 接收可能为 null 的列）。
type conversationRow struct {
	ConversationID int64
	UserA          int64
	UserB          int64
	LastSeq        int64
	LastMessageAt  time.Time
	ConvCreatedAt  time.Time
	ReadSeq        int64
	ClearedSeq     int64
	MessageID      *int64
	MessageSeq     *int64
	SenderID       *int64
	ClientMsgID    *string
	Type           *string
	Content        *string
	CreatedAt      *time.Time
}

func (r *gormRepository) ListConversations(ctx context.Context, userID int64) ([]ConversationSummary, error) {
	// 最后一条消息要排除我清空、删除的（FR-14、FR-20），不能再直接按 seq = last_seq 取，
	// 改成 LATERAL 子查询：对每个会话，从最新的一条往前找第一条我能看到的消息。
	// LATERAL 让子查询能引用外层这一行的列（c.id、cm.cleared_seq），类似对每一行执行一次关联子查询；
	// 走 uq_messages_conversation_seq 倒序扫描，通常第一条就满足，不会扫很多行。
	// LEFT JOIN ... ON true：找不到（全部删光）时会话照样返回，消息列为 NULL。
	// last_seq > 0 过滤掉"建了会话但还没有消息"的情况（此时 last_message_at 为 NULL）。
	var rows []conversationRow
	err := r.db.WithContext(ctx).Raw(`
SELECT c.id AS conversation_id, c.user_a, c.user_b, c.last_seq, c.last_message_at, c.created_at AS conv_created_at,
       cm.read_seq, cm.cleared_seq,
       m.id AS message_id, m.seq AS message_seq, m.sender_id, m.client_msg_id, m.type, m.content, m.created_at
FROM conversation_members cm
JOIN conversations c ON c.id = cm.conversation_id
LEFT JOIN LATERAL (
    SELECT * FROM messages m
    WHERE m.conversation_id = c.id AND m.seq > cm.cleared_seq
      AND NOT EXISTS (SELECT 1 FROM message_deletions d WHERE d.user_id = cm.user_id AND d.message_id = m.id)
    ORDER BY m.seq DESC
    LIMIT 1
) m ON true
WHERE cm.user_id = ? AND c.last_seq > 0
ORDER BY c.last_message_at DESC, c.id DESC`, userID).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	out := make([]ConversationSummary, 0, len(rows))
	for _, row := range rows {
		lastAt := row.LastMessageAt
		sum := ConversationSummary{
			Conversation: Conversation{
				ID:            row.ConversationID,
				UserA:         row.UserA,
				UserB:         row.UserB,
				LastSeq:       row.LastSeq,
				LastMessageAt: &lastAt,
				CreatedAt:     row.ConvCreatedAt,
			},
			ReadSeq:    row.ReadSeq,
			ClearedSeq: row.ClearedSeq,
		}
		if row.MessageID != nil {
			sum.LastMessage = &Message{
				ID:             *row.MessageID,
				ConversationID: row.ConversationID,
				Seq:            *row.MessageSeq,
				SenderID:       *row.SenderID,
				ClientMsgID:    *row.ClientMsgID,
				Type:           *row.Type,
				Content:        *row.Content,
				CreatedAt:      *row.CreatedAt,
			}
		}
		out = append(out, sum)
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

// visibleTo 在 messages（别名 m）上加"userID 能看到"的条件：不在他的清空位置之前，也没被他删除（FR-20）。
// 清空位置用标量子查询取：userID 不是会话成员时子查询为 NULL，m.seq > NULL 不成立，一条都查不到。
// NOT EXISTS 走 message_deletions 的主键 (user_id, message_id)。
func visibleTo(db *gorm.DB, userID int64) *gorm.DB {
	return db.Table("messages AS m").Select("m.*").
		Where("m.seq > (SELECT cm.cleared_seq FROM conversation_members cm WHERE cm.conversation_id = m.conversation_id AND cm.user_id = ?)", userID).
		Where("NOT EXISTS (SELECT 1 FROM message_deletions d WHERE d.user_id = ? AND d.message_id = m.id)", userID)
}

func (r *gormRepository) ListAfter(ctx context.Context, conversationID, userID, afterSeq int64, limit int) ([]Message, error) {
	var msgs []Message
	// 走唯一索引 uq_messages_conversation_seq 做范围扫描
	err := visibleTo(r.db.WithContext(ctx), userID).
		Where("m.conversation_id = ? AND m.seq > ?", conversationID, afterSeq).
		Order("m.seq ASC").
		Limit(limit).
		Find(&msgs).Error
	return msgs, err
}

func (r *gormRepository) ListBefore(ctx context.Context, conversationID, userID, beforeSeq int64, limit int) ([]Message, error) {
	q := visibleTo(r.db.WithContext(ctx), userID).Where("m.conversation_id = ?", conversationID)
	if beforeSeq > 0 {
		q = q.Where("m.seq < ?", beforeSeq)
	}
	var msgs []Message
	// 取"最近的 limit 条"要倒序查，再在内存里翻转成升序，调用方统一按 seq 升序处理
	if err := q.Order("m.seq DESC").Limit(limit).Find(&msgs).Error; err != nil {
		return nil, err
	}
	slices.Reverse(msgs)
	return msgs, nil
}

func (r *gormRepository) DeleteMessages(ctx context.Context, conversationID, userID int64, seqs []int64) ([]int64, error) {
	var deleted []int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 同一用户的删除串行执行，保证删除记录的 id 按提交顺序递增。
		// 为什么需要：id 来自序列，是在插入时分配的，不是提交时。两个标签页同时删除，可能 id = 10 的事务先提交、
		// id = 9 的后提交；另一台设备如果恰好在两次提交之间按游标同步，拿到 10 就把游标推到 10，
		// 之后 9 提交了也永远同步不到。加锁后，后一个事务要等前一个提交才能分配 id，id 顺序 = 提交顺序。
		// pg_advisory_xact_lock 是 PostgreSQL 的"咨询锁"：按一个整数加锁，事务结束自动释放，不锁任何表行。
		// 键直接用用户 ID；目前项目里只有这一处用咨询锁，不会和别的用途撞键。
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", userID).Error; err != nil {
			return err
		}
		// 一条 SQL 完成"按 seq 找到消息 → 插入删除记录 → 取回这次真正插入的 seq"：
		// ON CONFLICT DO NOTHING 跳过已经删过的（主键冲突），RETURNING 只返回真正插入的行。
		return tx.Raw(`
WITH ins AS (
    INSERT INTO message_deletions (user_id, message_id)
    SELECT ?, m.id FROM messages m WHERE m.conversation_id = ? AND m.seq IN ?
    ORDER BY m.seq -- 同一批删除的记录按 seq 顺序分配 id，同步时的顺序稳定
    ON CONFLICT DO NOTHING
    RETURNING message_id
)
SELECT m.seq FROM ins JOIN messages m ON m.id = ins.message_id ORDER BY m.seq`,
			userID, conversationID, seqs).Scan(&deleted).Error
	})
	return deleted, err
}

func (r *gormRepository) Clear(ctx context.Context, conversationID, userID, upToSeq int64) (int64, int64, bool, error) {
	db := r.db.WithContext(ctx)

	// 和 MarkRead 同样的写法：只在清空位置"确实会变大"时才更新，重复点击不产生写入、不触发推送。
	// read_seq 一起推进（清空视为已读）：被清掉的未读消息再也看不到，不推进的话未读数永远消不掉。
	var row struct {
		ClearedSeq int64
		ReadSeq    int64
	}
	res := db.Raw(`
UPDATE conversation_members cm
SET cleared_seq = LEAST(?, c.last_seq),
    read_seq    = GREATEST(cm.read_seq, LEAST(?, c.last_seq))
FROM conversations c
WHERE c.id = cm.conversation_id AND cm.conversation_id = ? AND cm.user_id = ?
  AND cm.cleared_seq < LEAST(?, c.last_seq)
RETURNING cm.cleared_seq, cm.read_seq`, upToSeq, upToSeq, conversationID, userID, upToSeq).Scan(&row)
	if res.Error != nil {
		return 0, 0, false, res.Error
	}
	if res.RowsAffected == 1 {
		return row.ClearedSeq, row.ReadSeq, true, nil
	}

	var m ConversationMember
	err := db.Where("conversation_id = ? AND user_id = ?", conversationID, userID).Take(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, 0, false, ErrConversationNotFound
	}
	if err != nil {
		return 0, 0, false, err
	}
	return m.ClearedSeq, m.ReadSeq, false, nil
}

func (r *gormRepository) ListDeletions(ctx context.Context, userID, afterID int64, limit int) ([]Deletion, error) {
	// 对方 ID 从会话的两个成员里取"不是我的那个"，和 Conversation.peerOf 一样
	var out []Deletion
	err := r.db.WithContext(ctx).Raw(`
SELECT d.id, m.seq, CASE WHEN c.user_a = ? THEN c.user_b ELSE c.user_a END AS peer_id
FROM message_deletions d
JOIN messages m ON m.id = d.message_id
JOIN conversations c ON c.id = m.conversation_id
WHERE d.user_id = ? AND d.id > ?
ORDER BY d.id
LIMIT ?`, userID, userID, afterID, limit).Scan(&out).Error
	return out, err
}
