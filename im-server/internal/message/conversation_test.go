package message

import (
	"errors"
	"slices"
	"testing"
)

// 会话列表（FR-14）和已读位置（FR-15）的 service 单元测试

func TestService_Send_AdvancesSenderReadSeq(t *testing.T) {
	svc, repo, _, _ := newTestService()

	// bob 连发 3 条，alice 一条没看
	for _, id := range []string{"b1", "b2", "b3"} {
		mustSend(t, svc, bob, alice, id, "hi")
	}
	res := mustSend(t, svc, alice, bob, "a1", "reply") // seq 4
	conv := res.Message.ConversationID

	// 发送即已读：alice 回复后，她的 read_seq 推进到自己这条消息，之前 bob 的 3 条视为已读
	if got := repo.readSeq(conv, alice); got != 4 {
		t.Errorf("alice read_seq = %d, want 4", got)
	}
	// bob 的 read_seq 停在他自己最后发的那条（3），alice 的回复对他是 1 条未读
	if got := repo.readSeq(conv, bob); got != 3 {
		t.Errorf("bob read_seq = %d, want 3", got)
	}

	// 重发同一条（去重）不改变任何人的已读位置
	if _, err := svc.Send(ctx, alice, bob, "a1", "reply"); err != nil {
		t.Fatal(err)
	}
	if repo.readSeq(conv, alice) != 4 || repo.readSeq(conv, bob) != 3 {
		t.Error("重复提交不应改变 read_seq")
	}
}

func TestService_ListConversations(t *testing.T) {
	svc, repo, friends, _ := newTestService()
	friends.set(alice, carol, true)

	mustSend(t, svc, alice, bob, "a1", "hi bob")
	mustSend(t, svc, bob, alice, "b1", "hi alice")
	mustSend(t, svc, bob, alice, "b2", "在吗")
	mustSend(t, svc, carol, alice, "c1", "hello") // 最后发生的会话，应排在最前

	// 一个只建了会话、还没有消息的会话不应出现在列表里
	if _, err := repo.EnsureConversation(ctx, alice, 1000000009); err != nil {
		t.Fatal(err)
	}

	views, err := svc.ListConversations(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("len = %d, want 2（不含没有消息的会话）", len(views))
	}

	first, second := views[0], views[1]
	// carol 不在 directory 里：只返回 ID，不报错
	if first.Peer.ID != carol || first.Peer.Username != "" {
		t.Errorf("first peer = %+v, want carol（只有 ID）", first.Peer)
	}
	if first.Unread != 1 || first.LastSeq != 1 || first.ReadSeq != 0 {
		t.Errorf("carol 会话 unread/lastSeq/readSeq = %d/%d/%d, want 1/1/0", first.Unread, first.LastSeq, first.ReadSeq)
	}

	if second.Peer.ID != bob || second.Peer.Username != "bob" || second.Peer.AvatarURL != "/a/bob.png" {
		t.Errorf("second peer = %+v, want bob 的资料", second.Peer)
	}
	// alice 发了 seq 1（read_seq 推进到 1），bob 发了 seq 2、3 → 2 条未读
	if second.Unread != 2 || second.LastSeq != 3 || second.ReadSeq != 1 {
		t.Errorf("bob 会话 unread/lastSeq/readSeq = %d/%d/%d, want 2/3/1", second.Unread, second.LastSeq, second.ReadSeq)
	}
	last := second.LastMessage
	if last.Content != "在吗" || last.Seq != 3 || last.SenderID != bob || last.RecipientID != alice {
		t.Errorf("bob 会话 lastMessage = %+v", last)
	}
	if !second.LastMessageAt.Equal(last.CreatedAt) {
		t.Errorf("lastMessageAt = %v, want %v", second.LastMessageAt, last.CreatedAt)
	}

	// bob 视角：只有和 alice 的会话；他自己发的最后一条不算未读
	bobViews, err := svc.ListConversations(ctx, bob)
	if err != nil {
		t.Fatal(err)
	}
	if len(bobViews) != 1 || bobViews[0].Peer.ID != alice || bobViews[0].Unread != 0 {
		t.Errorf("bob views = %+v, want 只有 alice 且未读 0", bobViews)
	}

	// 没有任何会话：返回空切片而不是 nil（JSON 输出 [] 而不是 null）
	empty, err := svc.ListConversations(ctx, 1000000008)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("empty = %#v, err = %v; want 空切片", empty, err)
	}
}

func TestService_MarkRead(t *testing.T) {
	svc, repo, _, n := newTestService()
	for _, id := range []string{"b1", "b2", "b3", "b4", "b5"} {
		mustSend(t, svc, bob, alice, id, "hi")
	}
	conv, _ := repo.FindConversation(ctx, alice, bob)

	steps := []struct {
		name    string
		seq     int64
		want    int64
		changed bool
	}{
		{"推进到 2", 2, 2, true},
		{"重复上报同一位置：不变、不推送", 2, 2, false},
		{"往回报（例如旧标签页迟到的请求）：只增不减", 1, 2, false},
		{"0 视为什么都没读：不变", 0, 2, false},
		{"推进到 4", 4, 4, true},
		{"超过 last_seq：按 last_seq 算", 99, 5, true},
		{"已经读到最新，再报更大的也不变", 100, 5, false},
	}
	for _, st := range steps {
		before := len(n.readEvents())
		got, err := svc.MarkRead(ctx, alice, bob, st.seq)
		if err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if got != st.want || repo.readSeq(conv.ID, alice) != st.want {
			t.Errorf("%s: readSeq = %d（库里 %d）, want %d", st.name, got, repo.readSeq(conv.ID, alice), st.want)
		}
		pushed := len(n.readEvents()) > before
		if pushed != st.changed {
			t.Errorf("%s: pushed = %v, want %v", st.name, pushed, st.changed)
		}
	}

	// 只推给自己，带上对方的 ID；bob 的已读位置不受影响
	want := []readPushed{{alice, bob, 2}, {alice, bob, 4}, {alice, bob, 5}}
	if got := n.readEvents(); !slices.Equal(got, want) {
		t.Errorf("read pushes = %v, want %v", got, want)
	}
	if got := repo.readSeq(conv.ID, bob); got != 5 {
		t.Errorf("bob read_seq = %d, want 5（他自己发的 5 条）", got)
	}
}

func TestService_MarkRead_Invalid(t *testing.T) {
	svc, _, _, n := newTestService()

	tests := []struct {
		name string
		peer int64
		seq  int64
		want error
	}{
		{"对方 ID 非法", 0, 1, ErrInvalidPeer},
		{"自己和自己", alice, 1, ErrInvalidPeer},
		{"seq 为负数", bob, -1, ErrInvalidReadSeq},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := svc.MarkRead(ctx, alice, tt.peer, tt.seq); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}

	// 还没聊过：返回 0，不报错，不推送
	got, err := svc.MarkRead(ctx, alice, carol, 3)
	if err != nil || got != 0 {
		t.Errorf("没有会话时 = (%d, %v), want (0, nil)", got, err)
	}
	if len(n.readEvents()) != 0 {
		t.Error("没有会话时不应推送")
	}
}
