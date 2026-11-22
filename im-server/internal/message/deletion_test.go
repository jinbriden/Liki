package message

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func pageSeqs(p *Page) []int64 {
	out := make([]int64, 0, len(p.Messages))
	for _, m := range p.Messages {
		out = append(out, m.Seq)
	}
	return out
}

// sendN alice、bob 交替发 n 条消息（好友之间），seq 为 1..n
func sendN(t *testing.T, svc *Service, n int) {
	t.Helper()
	for i := range n {
		from, to := alice, bob
		if i%2 == 1 {
			from, to = bob, alice
		}
		mustSend(t, svc, from, to, fmt.Sprintf("m%d", i), fmt.Sprintf("msg %d", i+1))
	}
}

// FR-19、FR-20：删除只对自己生效，拉取历史时过滤，重复删除幂等，只在有新删除时推送给自己
func TestService_DeleteMessages(t *testing.T) {
	svc, _, _, n := newTestService()
	sendN(t, svc, 5)

	if err := svc.DeleteMessages(ctx, alice, bob, []int64{4, 2, 4}); err != nil { // 乱序、重复都可以
		t.Fatal(err)
	}
	p, _ := svc.ListBefore(ctx, alice, bob, 0, 10)
	if got := pageSeqs(p); !slices.Equal(got, []int64{1, 3, 5}) || p.LastSeq != 5 {
		t.Errorf("alice 看到 %v lastSeq=%d, want [1 3 5] 5", got, p.LastSeq)
	}
	p, _ = svc.ListAfter(ctx, alice, bob, 1, 10)
	if got := pageSeqs(p); !slices.Equal(got, []int64{3, 5}) {
		t.Errorf("alice 增量同步 %v, want [3 5]", got)
	}
	p, _ = svc.ListBefore(ctx, bob, alice, 0, 10)
	if got := pageSeqs(p); !slices.Equal(got, []int64{1, 2, 3, 4, 5}) {
		t.Errorf("bob 应不受影响：%v", got)
	}

	if len(n.deletes) != 1 || n.deletes[0].userID != alice || n.deletes[0].peerID != bob || !slices.Equal(n.deletes[0].seqs, []int64{2, 4}) {
		t.Fatalf("推送 = %+v, want 一次 {alice, bob, [2 4]}", n.deletes)
	}
	// 再删一次：成功，但没有新删除，不推送
	if err := svc.DeleteMessages(ctx, alice, bob, []int64{2}); err != nil {
		t.Errorf("重复删除应成功：%v", err)
	}
	if len(n.deletes) != 1 {
		t.Errorf("重复删除不应推送：%d 次", len(n.deletes))
	}
}

func TestService_DeleteMessages_Invalid(t *testing.T) {
	svc, _, _, n := newTestService()
	sendN(t, svc, 3)

	many := make([]int64, MaxDeleteBatch+1)
	for i := range many {
		many[i] = int64(i + 1)
	}
	dupTo30 := append(slices.Clone(many[:MaxDeleteBatch]), 1) // 31 个元素，去重后 30 个
	tests := []struct {
		name string
		peer int64
		seqs []int64
		want error
	}{
		{"空列表", bob, nil, ErrInvalidDeleteCount},
		{"超过 30 条", bob, many, ErrInvalidDeleteCount},
		{"seq 为 0", bob, []int64{0, 1}, ErrInvalidSeq},
		{"seq 超过 last_seq", bob, []int64{1, 4}, ErrInvalidSeq},
		{"没聊过的人", carol, []int64{1}, ErrInvalidSeq},
		{"自己", alice, []int64{1}, ErrInvalidPeer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := svc.DeleteMessages(ctx, alice, tt.peer, tt.seqs); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if len(n.deletes) != 0 {
		t.Errorf("被拒绝的删除不应推送")
	}
	// 去重后不超过 30 条就可以（这里 seq 超出范围，所以是 ErrInvalidSeq 而不是条数错误）
	if err := svc.DeleteMessages(ctx, alice, bob, dupTo30); !errors.Is(err, ErrInvalidSeq) {
		t.Errorf("去重后 30 条：err = %v, want ErrInvalidSeq（条数合法）", err)
	}
}

// FR-14：会话列表的最后一条消息排除自己删除的；全部删光后会话仍在列表中，lastMessage 为 nil
func TestService_ListConversations_AfterDelete(t *testing.T) {
	svc, _, _, _ := newTestService()
	sendN(t, svc, 3)

	if err := svc.DeleteMessages(ctx, alice, bob, []int64{3}); err != nil {
		t.Fatal(err)
	}
	views, _ := svc.ListConversations(ctx, alice)
	if len(views) != 1 || views[0].LastMessage == nil || views[0].LastMessage.Seq != 2 || views[0].LastSeq != 3 {
		t.Fatalf("alice：lastMessage 应退到 seq 2，lastSeq 仍为 3：%+v", views)
	}
	if views[0].LastMessage.RecipientID != alice { // seq 2 是 bob 发给 alice 的
		t.Errorf("RecipientID = %d, want alice", views[0].LastMessage.RecipientID)
	}
	bobViews, _ := svc.ListConversations(ctx, bob)
	if bobViews[0].LastMessage.Seq != 3 {
		t.Errorf("bob 不受影响：lastMessage.seq = %d", bobViews[0].LastMessage.Seq)
	}

	_ = svc.DeleteMessages(ctx, alice, bob, []int64{1, 2})
	views, _ = svc.ListConversations(ctx, alice)
	if len(views) != 1 || views[0].LastMessage != nil || views[0].LastSeq != 3 {
		t.Errorf("全部删光：会话仍在、lastMessage 为 nil：%+v", views)
	}
}

// FR-19：清空到客户端已知的位置，同时视为已读；只增不减、超过 last_seq 按 last_seq 算；之后的新消息照常可见
func TestService_Clear(t *testing.T) {
	svc, _, _, n := newTestService()
	mustSend(t, svc, alice, bob, "a1", "hi")
	for i := range 3 {
		mustSend(t, svc, bob, alice, fmt.Sprintf("b%d", i), "?") // seq 2..4，alice 未读 3 条
	}

	res, err := svc.Clear(ctx, alice, bob, 3) // 点清空时 alice 只知道 seq 3，seq 4 不受影响
	if err != nil || res.ClearedSeq != 3 || res.ReadSeq != 3 {
		t.Fatalf("Clear(3) = %+v, %v; want cleared 3 read 3", res, err)
	}
	p, _ := svc.ListBefore(ctx, alice, bob, 0, 10)
	if got := pageSeqs(p); !slices.Equal(got, []int64{4}) || p.HasMore {
		t.Errorf("清空后 alice 看到 %v hasMore=%v, want [4] false", got, p.HasMore)
	}
	views, _ := svc.ListConversations(ctx, alice)
	if v := views[0]; v.ClearedSeq != 3 || v.Unread != 1 || v.LastMessage.Seq != 4 {
		t.Errorf("会话列表：cleared=%d unread=%d last=%d, want 3 1 4", v.ClearedSeq, v.Unread, v.LastMessage.Seq)
	}
	p, _ = svc.ListBefore(ctx, bob, alice, 0, 10)
	if len(p.Messages) != 4 {
		t.Errorf("bob 不受影响：%v", pageSeqs(p))
	}

	// 往回清空不生效，也不推送；超过 last_seq 按 last_seq 算
	if res, _ := svc.Clear(ctx, alice, bob, 1); res.ClearedSeq != 3 {
		t.Errorf("只增不减：cleared = %d, want 3", res.ClearedSeq)
	}
	if res, _ := svc.Clear(ctx, alice, bob, 99); res.ClearedSeq != 4 || res.ReadSeq != 4 {
		t.Errorf("Clear(99) = %+v, want cleared 4 read 4", res)
	}
	if len(n.clears) != 2 || n.clears[0] != (clearPushed{alice, bob, 3, 3}) {
		t.Errorf("推送 = %+v, want 两次（3、4），往回清空那次不推", n.clears)
	}

	// 全部清空后会话仍在，lastMessage 为 nil；新消息照常可见
	views, _ = svc.ListConversations(ctx, alice)
	if len(views) != 1 || views[0].LastMessage != nil {
		t.Errorf("清空后会话应仍在、摘要为空：%+v", views)
	}
	mustSend(t, svc, bob, alice, "b9", "new")
	p, _ = svc.ListBefore(ctx, alice, bob, 0, 10)
	if got := pageSeqs(p); !slices.Equal(got, []int64{5}) {
		t.Errorf("清空后的新消息：%v, want [5]", got)
	}

	if res, err := svc.Clear(ctx, alice, carol, 5); err != nil || *res != (ClearResult{}) {
		t.Errorf("没聊过：%+v, %v; want 全 0", res, err)
	}
	if _, err := svc.Clear(ctx, alice, bob, -1); !errors.Is(err, ErrInvalidClearSeq) {
		t.Errorf("负数：err = %v", err)
	}
}

// FR-20：删除记录按游标分页同步，只返回自己的
func TestService_ListDeletions(t *testing.T) {
	svc, _, friends, _ := newTestService()
	friends.set(alice, carol, true)
	sendN(t, svc, 3)
	mustSend(t, svc, alice, carol, "c1", "hi")

	_ = svc.DeleteMessages(ctx, alice, bob, []int64{1})
	_ = svc.DeleteMessages(ctx, bob, alice, []int64{2}) // bob 的，不应出现在 alice 的同步里
	_ = svc.DeleteMessages(ctx, alice, carol, []int64{1})
	_ = svc.DeleteMessages(ctx, alice, bob, []int64{3})

	items, hasMore, err := svc.ListDeletions(ctx, alice, 0, 2)
	if err != nil || len(items) != 2 || !hasMore {
		t.Fatalf("第一页 %+v hasMore=%v err=%v, want 2 条 true", items, hasMore, err)
	}
	if items[0].PeerID != bob || items[0].Seq != 1 || items[1].PeerID != carol || items[1].Seq != 1 {
		t.Errorf("第一页 = %+v", items)
	}
	items, hasMore, _ = svc.ListDeletions(ctx, alice, items[1].ID, 2)
	if len(items) != 1 || hasMore || items[0].PeerID != bob || items[0].Seq != 3 {
		t.Errorf("第二页 = %+v hasMore=%v, want [{bob 3}] false", items, hasMore)
	}
}
