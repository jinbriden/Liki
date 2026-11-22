package message

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

var ctx = context.Background()

const (
	alice int64 = 1000000001
	bob   int64 = 1000000002
	carol int64 = 1000000003
)

// testDirectory carol 故意不在里面：验证查不到资料时会话列表只返回 ID，不报错
var testDirectory = fakeDirectory{
	alice: {ID: alice, Username: "alice"},
	bob:   {ID: bob, Username: "bob", AvatarURL: "/a/bob.png"},
}

func newTestService() (*Service, *fakeRepo, fakeFriends, *fakeNotifier) {
	repo := newFakeRepo()
	friends := fakeFriends{}
	friends.set(alice, bob, true)
	n := &fakeNotifier{}
	return NewService(repo, friends, testDirectory, n), repo, friends, n
}

func mustSend(t *testing.T, svc *Service, from, to int64, clientMsgID, content string) *SendResult {
	t.Helper()
	res, err := svc.Send(ctx, from, to, clientMsgID, content)
	if err != nil {
		t.Fatalf("Send(%d→%d, %s): %v", from, to, clientMsgID, err)
	}
	return res
}

func TestService_Send_Validation(t *testing.T) {
	svc, repo, _, n := newTestService()

	tests := []struct {
		name        string
		to          int64
		clientMsgID string
		content     string
		want        error
	}{
		{"空内容", bob, "c1", "", ErrEmptyContent},
		{"只有空白", bob, "c1", " \n\t ", ErrEmptyContent},
		{"超过 1500 字（中文按字符数计）", bob, "c1", strings.Repeat("字", MaxContentLen+1), ErrContentTooLong},
		{"包含 NUL 字符", bob, "c1", "a\x00b", ErrInvalidContent},
		{"缺少 clientMsgId", bob, "", "hi", ErrInvalidClientMsgID},
		{"clientMsgId 太长", bob, strings.Repeat("a", 65), "hi", ErrInvalidClientMsgID},
		{"clientMsgId 含非法字符", bob, "a b", "hi", ErrInvalidClientMsgID},
		{"发给自己", alice, "c1", "hi", ErrInvalidPeer},
		{"接收方 ID 非法", 0, "c1", "hi", ErrInvalidPeer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := svc.Send(ctx, alice, tt.to, tt.clientMsgID, tt.content); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if len(repo.msgs) != 0 || n.count() != 0 {
		t.Errorf("被拒绝的消息不应写库或推送：msgs = %d, pushes = %d", len(repo.msgs), n.count())
	}
}

func TestService_Send_BoundaryLength(t *testing.T) {
	svc, _, _, _ := newTestService()
	// 恰好 1500 个字符可以发送；emoji 在 Go 里是 1 个 rune，按 1 个字符计
	content := strings.Repeat("😀", MaxContentLen)
	res := mustSend(t, svc, alice, bob, "c1", content)
	if res.Message.Content != content {
		t.Error("内容应原样保存")
	}
}

func TestService_Send_SeqAndPush(t *testing.T) {
	svc, _, _, n := newTestService()

	r1 := mustSend(t, svc, alice, bob, "a1", "hi")
	r2 := mustSend(t, svc, bob, alice, "b1", "hello")
	r3 := mustSend(t, svc, alice, bob, "a2", "  保留首尾空格  ")

	// 双方共用一个会话，seq 在会话内从 1 连续递增，不分发送方
	if r1.Message.ConversationID != r2.Message.ConversationID {
		t.Fatalf("A→B 和 B→A 应在同一个会话：%d vs %d", r1.Message.ConversationID, r2.Message.ConversationID)
	}
	if got := []int64{r1.Message.Seq, r2.Message.Seq, r3.Message.Seq}; !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("seq = %v, want [1 2 3]", got)
	}
	if r1.Message.RecipientID != bob || r2.Message.RecipientID != alice {
		t.Errorf("RecipientID = %d, %d", r1.Message.RecipientID, r2.Message.RecipientID)
	}
	if r3.Message.Content != "  保留首尾空格  " {
		t.Errorf("content = %q, 应原样保存", r3.Message.Content)
	}

	// 每条新消息推送给接收方和发送方自己（多处登录同步）
	if len(n.events) != 3 {
		t.Fatalf("pushes = %d, want 3", len(n.events))
	}
	if got := n.events[0].recipients; !slices.Equal(got, []int64{bob, alice}) {
		t.Errorf("第一条消息推送给 %v, want [bob alice]", got)
	}
}

func TestService_Send_DuplicateIsIdempotent(t *testing.T) {
	svc, repo, friends, n := newTestService()

	first := mustSend(t, svc, alice, bob, "dup", "hi")
	// 客户端没收到 ACK，用同一个 clientMsgId 重发（内容即使不同也以第一次为准）
	second := mustSend(t, svc, alice, bob, "dup", "hi again")

	if !second.Duplicate || second.Message.ID != first.Message.ID || second.Message.Seq != first.Message.Seq {
		t.Errorf("重发应返回已有消息：first = %+v, second = %+v", first.Message, second.Message)
	}
	if second.Message.RecipientID != bob {
		t.Errorf("重发返回的 RecipientID = %d, want bob", second.Message.RecipientID)
	}
	if len(repo.msgs) != 1 || n.count() != 1 {
		t.Errorf("重发不应重复写库或推送：msgs = %d, pushes = %d", len(repo.msgs), n.count())
	}

	// 已写库的消息在解除好友后重发，仍应确认成功（ACK 丢了而已）
	friends.set(alice, bob, false)
	third := mustSend(t, svc, alice, bob, "dup", "hi")
	if !third.Duplicate || third.Message.Seq != first.Message.Seq {
		t.Errorf("解除好友后重发已写入的消息：%+v", third)
	}

	// clientMsgId 按发送方区分：别人用同一个 ID 不算重复
	friends.set(alice, bob, true)
	other := mustSend(t, svc, bob, alice, "dup", "from bob")
	if other.Duplicate || other.Message.Seq != 2 {
		t.Errorf("不同发送方的相同 clientMsgId 应是新消息：%+v", other)
	}
}

// 去重查不到之后、插入之前，同一条消息的另一次提交抢先写入（两次重发挤在一起）
func TestService_Send_DuplicateRace(t *testing.T) {
	svc, repo, _, n := newTestService()
	mustSend(t, svc, alice, bob, "warmup", "x") // 先建好会话

	var winner *Message
	repo.beforeCreate = func(m *Message) {
		repo.beforeCreate = nil // 只触发一次
		cp := *m
		if err := repo.Create(ctx, &cp, false); err != nil {
			t.Fatalf("抢先写入：%v", err)
		}
		winner = &cp
	}

	res := mustSend(t, svc, alice, bob, "race", "hi")
	if !res.Duplicate || res.Message.ID != winner.ID {
		t.Errorf("应返回抢先写入的那条：got %+v, winner %+v", res.Message, winner)
	}
	// 失败的那次不能占用 seq：会话里只有 warmup(1) 和 race(2)，没有空洞
	conv, _ := repo.FindConversation(ctx, alice, bob)
	if conv.LastSeq != 2 || winner.Seq != 2 {
		t.Errorf("last_seq = %d, winner.seq = %d, want 2, 2", conv.LastSeq, winner.Seq)
	}
	if n.count() != 1 { // 只有 warmup 走到了推送；抢先写入的那次是直接调 repo 模拟的
		t.Errorf("pushes = %d, want 1", n.count())
	}
}

// FR-12：非好友之间，对方回复前只能发一条；对方回复一条后可以再发一条
func TestService_Send_StrangerLimit(t *testing.T) {
	svc, repo, _, n := newTestService() // alice 和 carol 不是好友

	first := mustSend(t, svc, alice, carol, "a1", "你好") // 第一条：会话还不存在，可以发
	if first.Message.Seq != 1 {
		t.Fatalf("seq = %d, want 1", first.Message.Seq)
	}
	if _, err := svc.Send(ctx, alice, carol, "a2", "在吗"); !errors.Is(err, ErrStrangerLimit) {
		t.Fatalf("对方回复前发第二条：err = %v, want ErrStrangerLimit", err)
	}

	// 重发已写入的第一条（ACK 丢了），应确认成功，而不是被当成"第二条"拒绝
	dup := mustSend(t, svc, alice, carol, "a1", "你好")
	if !dup.Duplicate || dup.Message.Seq != 1 {
		t.Errorf("重发已写入的消息：%+v", dup)
	}

	mustSend(t, svc, carol, alice, "c1", "你是？") // 对方回复
	if r := mustSend(t, svc, alice, carol, "a3", "我是 alice"); r.Message.Seq != 3 {
		t.Errorf("对方回复后可以再发一条：seq = %d, want 3（被拒绝的 a2 不能占用 seq）", r.Message.Seq)
	}
	if _, err := svc.Send(ctx, alice, carol, "a4", "还在吗"); !errors.Is(err, ErrStrangerLimit) {
		t.Errorf("回复后只解锁一条：err = %v, want ErrStrangerLimit", err)
	}
	// 被拒绝的两条既不写库也不推送
	if len(repo.msgs) != 3 || n.count() != 3 {
		t.Errorf("msgs = %d, pushes = %d, want 3, 3", len(repo.msgs), n.count())
	}
}

// FR-08 + FR-12：删除好友后，老会话按非好友处理（看会话里最后一条是谁发的）；好友之间不受限
func TestService_Send_StrangerLimitAfterUnfriend(t *testing.T) {
	svc, _, friends, _ := newTestService()

	// 好友之间连发不受限
	for i := range 3 {
		mustSend(t, svc, alice, bob, fmt.Sprintf("a%d", i), "hi")
	}

	friends.set(alice, bob, false)
	if _, err := svc.Send(ctx, alice, bob, "a9", "hi"); !errors.Is(err, ErrStrangerLimit) {
		t.Fatalf("解除好友后，最后一条是自己发的：err = %v, want ErrStrangerLimit", err)
	}
	mustSend(t, svc, bob, alice, "b1", "嗯") // 最后一条是 alice 的，bob 可以发一条
	if _, err := svc.Send(ctx, bob, alice, "b2", "?"); !errors.Is(err, ErrStrangerLimit) {
		t.Errorf("bob 第二条：err = %v, want ErrStrangerLimit", err)
	}
	mustSend(t, svc, alice, bob, "a10", "hi") // bob 回复了，alice 可以再发一条

	// 重新成为好友后恢复不受限
	friends.set(alice, bob, true)
	mustSend(t, svc, alice, bob, "a11", "hi")
	mustSend(t, svc, alice, bob, "a12", "hi")
}

// 并发发送：seq 不重复、不跳号（fake 里用锁模拟数据库行锁；真实数据库的并发测试见 repository_test.go）
func TestService_Send_ConcurrentSeq(t *testing.T) {
	svc, repo, _, _ := newTestService()
	const n = 50

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			from, to := alice, bob
			if i%2 == 1 {
				from, to = bob, alice
			}
			if _, err := svc.Send(ctx, from, to, fmt.Sprintf("c%d", i), "hi"); err != nil {
				t.Errorf("Send: %v", err)
			}
		}()
	}
	wg.Wait()

	seqs := make([]int64, 0, n)
	for _, m := range repo.msgs {
		seqs = append(seqs, m.Seq)
	}
	slices.Sort(seqs)
	for i, s := range seqs {
		if s != int64(i+1) {
			t.Fatalf("seq 应为 1..%d 连续不重复，got %v", n, seqs)
		}
	}
}

func TestService_ListAfterAndBefore(t *testing.T) {
	svc, _, _, _ := newTestService()
	for i := range 7 {
		mustSend(t, svc, alice, bob, fmt.Sprintf("m%d", i), fmt.Sprintf("msg %d", i+1))
	}

	seqsOf := func(p *Page) []int64 {
		out := make([]int64, 0, len(p.Messages))
		for _, m := range p.Messages {
			out = append(out, m.Seq)
		}
		return out
	}

	// 增量同步：本地最大 seq 为 2，每页 3 条
	p, err := svc.ListAfter(ctx, bob, alice, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := seqsOf(p); !slices.Equal(got, []int64{3, 4, 5}) || !p.HasMore {
		t.Errorf("ListAfter(2) = %v hasMore=%v, want [3 4 5] true", got, p.HasMore)
	}
	p, _ = svc.ListAfter(ctx, bob, alice, 5, 3)
	if got := seqsOf(p); !slices.Equal(got, []int64{6, 7}) || p.HasMore {
		t.Errorf("ListAfter(5) = %v hasMore=%v, want [6 7] false", got, p.HasMore)
	}
	if p.Messages[0].RecipientID != bob {
		t.Errorf("RecipientID = %d, want bob", p.Messages[0].RecipientID)
	}

	// 最新一页，再向前翻
	p, _ = svc.ListBefore(ctx, alice, bob, 0, 3)
	if got := seqsOf(p); !slices.Equal(got, []int64{5, 6, 7}) || !p.HasMore {
		t.Errorf("ListBefore(latest) = %v hasMore=%v, want [5 6 7] true", got, p.HasMore)
	}
	p, _ = svc.ListBefore(ctx, alice, bob, 2, 3)
	if got := seqsOf(p); !slices.Equal(got, []int64{1}) || p.HasMore {
		t.Errorf("ListBefore(2) = %v hasMore=%v, want [1] false", got, p.HasMore)
	}

	// 没聊过的人：空页；查自己：参数错误
	p, err = svc.ListAfter(ctx, alice, carol, 0, 10)
	if err != nil || len(p.Messages) != 0 || p.HasMore {
		t.Errorf("没有会话时应返回空页：%+v, %v", p, err)
	}
	if _, err := svc.ListBefore(ctx, alice, alice, 0, 10); !errors.Is(err, ErrInvalidPeer) {
		t.Errorf("查自己：err = %v, want ErrInvalidPeer", err)
	}
}

// countLimiter 前 n 次放行，之后拒绝
type countLimiter struct {
	mu   sync.Mutex
	n    int
	used int
}

func (l *countLimiter) Allow(context.Context, string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.used++
	return l.used <= l.n
}

// NFR-05：发消息限流。重发命中去重的不计数，超出后返回 ErrRateLimited 且不写库
func TestService_Send_RateLimited(t *testing.T) {
	repo := newFakeRepo()
	friends := fakeFriends{}
	friends.set(alice, bob, true)
	lim := &countLimiter{n: 2}
	svc := NewService(repo, friends, testDirectory, &fakeNotifier{}, WithSendLimiter(lim))

	mustSend(t, svc, alice, bob, "a1", "hi")
	mustSend(t, svc, alice, bob, "a1", "hi") // 重发：命中去重，不消耗配额
	mustSend(t, svc, alice, bob, "a2", "hi")
	if _, err := svc.Send(ctx, alice, bob, "a3", "hi"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("第 3 条新消息应被限流：%v", err)
	}
	if lim.used != 3 || len(repo.msgs) != 2 {
		t.Errorf("配额消耗 %d 次（want 3：a1、a2、a3），写库 %d 条（want 2）", lim.used, len(repo.msgs))
	}
	// 已经写库的消息即使在限流期间重发，也照样确认
	if res := mustSend(t, svc, alice, bob, "a2", "hi"); !res.Duplicate {
		t.Error("限流期间重发已写入的消息应返回 Duplicate")
	}
}
