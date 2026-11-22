package message

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"sync"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// 这里的测试连接真实的 PostgreSQL，验证 fake 模拟不了的东西：行锁让并发分配的 seq 不重复、
// 唯一约束去重、事务回滚不留 seq 空洞。
//
// 需要一个独立的测试库（不要用开发库 im，测试会插入和删除数据）：
//
//	docker exec im-postgres psql -U im -d im -c "CREATE DATABASE im_test"
//	migrate -path migrations -database "postgres://im:im123@localhost:5432/im_test?sslmode=disable" up
//	TEST_DATABASE_DSN="host=localhost user=im password=im123 dbname=im_test port=5432 sslmode=disable" go test ./internal/message/
//
// 没设置 TEST_DATABASE_DSN 时跳过。

type nopNotifier struct{}

func (nopNotifier) MessageCreated(context.Context, *Message, []int64) {}
func (nopNotifier) ReadUpdated(context.Context, int64, int64, int64)  {}

type allFriends struct{}

func (allFriends) AreFriends(context.Context, int64, int64) (bool, error) { return true, nil }

// setupDB 连接测试库，插入两个临时用户，测试结束后删除它们产生的所有数据
func setupDB(t *testing.T) (*gorm.DB, int64, int64) {
	t.Helper()
	db, ids := setupDBUsers(t, 2)
	return db, ids[0], ids[1]
}

// setupDBUsers 同 setupDB，插入 n 个临时用户（ID 连续）
func setupDBUsers(t *testing.T, n int) (*gorm.DB, []int64) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("TEST_DATABASE_DSN not set, skipping database test")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		TranslateError: true,
		Logger:         gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// 随机 10 位 ID，避免和库里已有数据、并行运行的其他测试冲突
	base := 1_000_000_000 + rand.Int64N(8_000_000_000)
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = base + int64(i)
		if err := db.Exec("INSERT INTO users (id, username, password_hash) VALUES (?, ?, 'x')", ids[i], fmt.Sprintf("t%d", ids[i])).Error; err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}
	t.Cleanup(func() {
		// 按外键依赖倒序删除。这是测试库，物理删除没问题（生产数据永不物理删除消息，FR-21）
		convs := db.Table("conversations").Select("id").Where("user_a IN ? OR user_b IN ?", ids, ids)
		db.Exec("DELETE FROM messages WHERE conversation_id IN (?)", convs)
		db.Exec("DELETE FROM conversation_members WHERE conversation_id IN (?)", convs)
		db.Exec("DELETE FROM conversations WHERE user_a IN ? OR user_b IN ?", ids, ids)
		db.Exec("DELETE FROM users WHERE id IN ?", ids)
	})
	return db, ids
}

func TestRepository_ConcurrentSend(t *testing.T) {
	db, a, b := setupDB(t)
	repo := NewRepository(db)
	svc := NewService(repo, allFriends{}, fakeDirectory{}, nopNotifier{})

	const n = 30
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			from, to := a, b
			if i%2 == 1 { // 两个方向同时发，包括两人同时发第一条消息、并发创建会话
				from, to = b, a
			}
			if _, err := svc.Send(ctx, from, to, fmt.Sprintf("c-%d", i), "hi"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Send: %v", err)
	}

	var convCount int64
	db.Table("conversations").Where("user_a = ? AND user_b = ?", min(a, b), max(a, b)).Count(&convCount)
	if convCount != 1 {
		t.Fatalf("conversations = %d, want 1", convCount)
	}
	conv, err := repo.FindConversation(ctx, a, b)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := repo.ListAfter(ctx, conv.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	seqs := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		seqs = append(seqs, m.Seq)
	}
	want := make([]int64, n)
	for i := range want {
		want[i] = int64(i + 1)
	}
	if !slices.Equal(seqs, want) || conv.LastSeq != n {
		t.Errorf("seq = %v, last_seq = %d; want 1..%d 连续不重复", seqs, conv.LastSeq, n)
	}
	if msgs[0].CreatedAt.IsZero() || conv.LastMessageAt == nil {
		t.Error("created_at / last_message_at 应由数据库 now() 写入")
	}
}

func TestRepository_ConcurrentDuplicate(t *testing.T) {
	db, a, b := setupDB(t)
	repo := NewRepository(db)
	svc := NewService(repo, allFriends{}, fakeDirectory{}, nopNotifier{})
	mustSend(t, svc, a, b, "first", "hi") // 先建好会话，seq = 1

	// 同一条消息被并发重发 10 次：全部返回成功、拿到同一个 seq，库里只有一条
	const n = 10
	results := make([]*SendResult, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := svc.Send(ctx, a, b, "same-id", "hello")
			if err != nil {
				t.Errorf("Send: %v", err)
				return
			}
			results[i] = res
		}()
	}
	wg.Wait()

	for _, r := range results {
		if r == nil || r.Message.Seq != 2 {
			t.Fatalf("所有重发都应返回 seq 2，got %+v", r)
		}
	}
	var count int64
	db.Model(&Message{}).Where("sender_id = ? AND client_msg_id = ?", a, "same-id").Count(&count)
	conv, _ := repo.FindConversation(ctx, a, b)
	// 冲突的事务回滚了 last_seq 的自增，所以 last_seq 仍是 2，下一条消息是 3，不会有空洞
	if count != 1 || conv.LastSeq != 2 {
		t.Errorf("rows = %d, last_seq = %d; want 1, 2", count, conv.LastSeq)
	}
	next := mustSend(t, svc, b, a, "next", "x")
	if next.Message.Seq != 3 {
		t.Errorf("下一条消息 seq = %d, want 3", next.Message.Seq)
	}
}

// 会话列表的 SQL（JOIN 最后一条消息、列名映射、排序）和发送即已读
func TestRepository_ListConversations(t *testing.T) {
	db, ids := setupDBUsers(t, 3)
	a, b, c := ids[0], ids[1], ids[2]
	repo := NewRepository(db)
	svc := NewService(repo, allFriends{}, fakeDirectory{}, nopNotifier{})

	mustSend(t, svc, b, a, "b1", "one")
	mustSend(t, svc, b, a, "b2", "two")
	mustSend(t, svc, a, b, "a1", "three") // a 回复：a 的 read_seq → 3
	mustSend(t, svc, b, a, "b3", "four")  // a 有 1 条未读
	mustSend(t, svc, c, a, "c1", "from c")
	if _, err := repo.EnsureConversation(ctx, b, c); err != nil { // 没有消息的会话
		t.Fatal(err)
	}

	sums, err := repo.ListConversations(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 2 {
		t.Fatalf("len = %d, want 2", len(sums))
	}
	if sums[0].Conversation.peerOf(a) != c || sums[1].Conversation.peerOf(a) != b {
		t.Errorf("顺序不对：want [c, b]")
	}
	ab := sums[1]
	if ab.Conversation.LastSeq != 4 || ab.ReadSeq != 3 {
		t.Errorf("lastSeq/readSeq = %d/%d, want 4/3", ab.Conversation.LastSeq, ab.ReadSeq)
	}
	m := ab.LastMessage
	if m.Seq != 4 || m.Content != "four" || m.SenderID != b || m.ClientMsgID != "b3" || m.Type != TypeText || m.ID == 0 {
		t.Errorf("lastMessage = %+v", m)
	}
	if m.CreatedAt.IsZero() || ab.Conversation.LastMessageAt == nil || !ab.Conversation.LastMessageAt.Equal(m.CreatedAt) {
		t.Errorf("created_at = %v, last_message_at = %v；应相同（同一事务的 now()）", m.CreatedAt, ab.Conversation.LastMessageAt)
	}

	// b 的视角：他最后发了 seq 4（b3），read_seq 随之推进到 4，没有未读
	bs, err := repo.ListConversations(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 || bs[0].ReadSeq != 4 {
		t.Errorf("b sums = %+v, want 1 个会话、read_seq 4", bs)
	}
}

// MarkRead 的条件更新：只增不减、不超过 last_seq、非成员
func TestRepository_MarkRead(t *testing.T) {
	db, ids := setupDBUsers(t, 3)
	a, b, c := ids[0], ids[1], ids[2]
	repo := NewRepository(db)
	svc := NewService(repo, allFriends{}, fakeDirectory{}, nopNotifier{})
	for i := range 3 {
		mustSend(t, svc, b, a, fmt.Sprintf("b%d", i), "hi")
	}
	conv, err := repo.FindConversation(ctx, a, b)
	if err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		seq     int64
		want    int64
		changed bool
	}{
		{2, 2, true},
		{2, 2, false},
		{1, 2, false},
		{50, 3, true}, // 超过 last_seq 按 last_seq 算
		{50, 3, false},
	}
	for _, st := range steps {
		got, changed, err := repo.MarkRead(ctx, conv.ID, a, st.seq)
		if err != nil {
			t.Fatalf("MarkRead(%d): %v", st.seq, err)
		}
		if got != st.want || changed != st.changed {
			t.Errorf("MarkRead(%d) = (%d, %v), want (%d, %v)", st.seq, got, changed, st.want, st.changed)
		}
	}

	if _, _, err := repo.MarkRead(ctx, conv.ID, c, 1); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("非成员 err = %v, want ErrConversationNotFound", err)
	}
}
