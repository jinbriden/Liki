package friend

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 测试用户 ID
const (
	alice int64 = 1000000001
	bob   int64 = 1000000002
	carol int64 = 1000000003
	dave  int64 = 1000000004
	erin  int64 = 1000000005
	ghost int64 = 1999999999 // 不存在的用户
)

var ctx = context.Background()

func defaultUsers() map[int64]string {
	return map[int64]string{alice: "alice", bob: "bob", carol: "carol", dave: "dave", erin: "erin"}
}

// mustSend 发送申请，失败时终止测试
func mustSend(t *testing.T, svc *Service, from, to int64) *FriendRequest {
	t.Helper()
	req, err := svc.SendRequest(ctx, from, to, "hi")
	if err != nil {
		t.Fatalf("SendRequest(%d → %d): %v", from, to, err)
	}
	return req
}

// makeFriends 走一遍"申请 + 同意"让两人成为好友
func makeFriends(t *testing.T, svc *Service, a, b int64) {
	t.Helper()
	req := mustSend(t, svc, a, b)
	if err := svc.Accept(ctx, b, req.ID); err != nil {
		t.Fatalf("Accept: %v", err)
	}
}

func assertFriends(t *testing.T, svc *Service, a, b int64, want bool) {
	t.Helper()
	for _, pair := range [][2]int64{{a, b}, {b, a}} {
		got, err := svc.AreFriends(ctx, pair[0], pair[1])
		if err != nil {
			t.Fatalf("AreFriends: %v", err)
		}
		if got != want {
			t.Errorf("AreFriends(%d, %d) = %v, want %v", pair[0], pair[1], got, want)
		}
	}
}

func TestService_SendRequest_Validation(t *testing.T) {
	tests := []struct {
		name    string
		to      int64
		message string
		wantErr error
		wantMsg string
	}{
		{name: "正常申请", to: bob, message: "你好", wantMsg: "你好"},
		{name: "验证信息为空", to: bob, message: ""},
		{name: "首尾空白被去掉", to: bob, message: "  你好 \n", wantMsg: "你好"},
		{name: "100个汉字", to: bob, message: strings.Repeat("好", 100), wantMsg: strings.Repeat("好", 100)},
		{name: "101个汉字", to: bob, message: strings.Repeat("好", 101), wantErr: ErrMessageTooLong},
		{name: "101个字符但首尾是空白", to: bob, message: strings.Repeat("a", 100) + " ", wantMsg: strings.Repeat("a", 100)},
		{name: "添加自己", to: alice, wantErr: ErrCannotAddSelf},
		{name: "目标用户不存在", to: ghost, wantErr: ErrTargetUserNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := newTestService(defaultUsers())
			req, err := svc.SendRequest(ctx, alice, tt.to, tt.message)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if req.ID == 0 || req.FromUserID != alice || req.ToUserID != tt.to || req.Status != StatusPending {
				t.Errorf("unexpected request: %+v", req)
			}
			if req.Message != tt.wantMsg {
				t.Errorf("message = %q, want %q", req.Message, tt.wantMsg)
			}
		})
	}
}

func TestService_SendRequest_AlreadyFriends(t *testing.T) {
	svc, _ := newTestService(defaultUsers())
	makeFriends(t, svc, alice, bob)

	for _, from := range []int64{alice, bob} { // 双方都不能再申请
		if _, err := svc.SendRequest(ctx, from, alice+bob-from, ""); !errors.Is(err, ErrAlreadyFriends) {
			t.Errorf("from %d: err = %v, want ErrAlreadyFriends", from, err)
		}
	}
}

func TestService_SendRequest_DuplicateIsIdempotent(t *testing.T) {
	svc, repo := newTestService(defaultUsers())
	first := mustSend(t, svc, alice, bob)
	second := mustSend(t, svc, alice, bob)

	if second.ID != first.ID {
		t.Errorf("重复申请应返回已有申请：first.ID = %d, second.ID = %d", first.ID, second.ID)
	}
	if n := len(repo.requests); n != 1 {
		t.Errorf("库里有 %d 条申请，want 1", n)
	}
}

func TestService_SendRequest_ReversePending(t *testing.T) {
	svc, _ := newTestService(defaultUsers())
	mustSend(t, svc, alice, bob)

	if _, err := svc.SendRequest(ctx, bob, alice, ""); !errors.Is(err, ErrReverseRequestPending) {
		t.Fatalf("err = %v, want ErrReverseRequestPending", err)
	}
}

func TestService_SendRequest_AfterReject(t *testing.T) {
	svc, _ := newTestService(defaultUsers())
	first := mustSend(t, svc, alice, bob)
	if err := svc.Reject(ctx, bob, first.ID); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	// 被拒绝后可以重新申请，是一条新申请
	second := mustSend(t, svc, alice, bob)
	if second.ID == first.ID {
		t.Errorf("被拒绝后重新申请应创建新申请，got 同一个 ID %d", second.ID)
	}
}

func TestService_Accept(t *testing.T) {
	svc, repo := newTestService(defaultUsers())
	req := mustSend(t, svc, alice, bob)

	if err := svc.Accept(ctx, bob, req.ID); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	assertFriends(t, svc, alice, bob, true)

	stored := repo.requests[req.ID]
	if stored.Status != StatusAccepted || stored.HandledAt == nil {
		t.Errorf("申请状态 = %s, handledAt = %v, want accepted 且 handledAt 非空", stored.Status, stored.HandledAt)
	}

	// 已处理的申请不能再处理
	if err := svc.Accept(ctx, bob, req.ID); !errors.Is(err, ErrRequestNotPending) {
		t.Errorf("重复同意：err = %v, want ErrRequestNotPending", err)
	}
	if err := svc.Reject(ctx, bob, req.ID); !errors.Is(err, ErrRequestNotPending) {
		t.Errorf("同意后拒绝：err = %v, want ErrRequestNotPending", err)
	}
}

func TestService_Reject(t *testing.T) {
	svc, repo := newTestService(defaultUsers())
	req := mustSend(t, svc, alice, bob)

	if err := svc.Reject(ctx, bob, req.ID); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	assertFriends(t, svc, alice, bob, false)
	if s := repo.requests[req.ID].Status; s != StatusRejected {
		t.Errorf("status = %s, want rejected", s)
	}
}

func TestService_Handle_NotYourRequest(t *testing.T) {
	svc, _ := newTestService(defaultUsers())
	req := mustSend(t, svc, alice, bob)

	tests := []struct {
		name      string
		me        int64
		requestID int64
	}{
		{name: "申请人自己同意", me: alice, requestID: req.ID},
		{name: "第三人处理", me: carol, requestID: req.ID},
		{name: "申请不存在", me: bob, requestID: 9999},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := svc.Accept(ctx, tt.me, tt.requestID); !errors.Is(err, ErrRequestNotFound) {
				t.Errorf("Accept: err = %v, want ErrRequestNotFound", err)
			}
			if err := svc.Reject(ctx, tt.me, tt.requestID); !errors.Is(err, ErrRequestNotFound) {
				t.Errorf("Reject: err = %v, want ErrRequestNotFound", err)
			}
		})
	}
	assertFriends(t, svc, alice, bob, false)
}

func TestService_DeleteFriend(t *testing.T) {
	svc, _ := newTestService(defaultUsers())
	makeFriends(t, svc, alice, bob)

	if err := svc.DeleteFriend(ctx, alice, bob); err != nil {
		t.Fatalf("DeleteFriend: %v", err)
	}
	assertFriends(t, svc, alice, bob, false) // 双向解除

	// 幂等：再删一次、删一个从来不是好友的人，都不报错
	if err := svc.DeleteFriend(ctx, alice, bob); err != nil {
		t.Errorf("重复删除：%v", err)
	}
	if err := svc.DeleteFriend(ctx, alice, carol); err != nil {
		t.Errorf("删除非好友：%v", err)
	}

	// 删除后可以重新申请（被删的一方也可以）
	makeFriends(t, svc, bob, alice)
	assertFriends(t, svc, alice, bob, true)
}

func TestService_Search(t *testing.T) {
	// 除了 erin，所有人都叫 Sam（用户名可以重复）
	svc, _ := newTestService(map[int64]string{
		alice: "Sam", bob: "sam", carol: "SAM", dave: "sam", erin: "erin",
	})
	makeFriends(t, svc, alice, bob) // bob：好友
	mustSend(t, svc, alice, carol)  // carol：我已申请
	mustSend(t, svc, dave, alice)   // dave：对方已申请我

	results, err := svc.Search(ctx, alice, "  sAm ")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := map[int64]Relation{}
	for _, r := range results {
		got[r.User.ID] = r.Relation
	}
	want := map[int64]Relation{bob: RelationFriend, carol: RelationPendingOut, dave: RelationPendingIn}
	if len(got) != len(want) {
		t.Fatalf("results = %v, want %v（不应包含自己）", got, want)
	}
	for id, rel := range want {
		if got[id] != rel {
			t.Errorf("user %d: relation = %q, want %q", id, got[id], rel)
		}
	}

	// 按 ID 搜索
	results, err = svc.Search(ctx, alice, "1000000005")
	if err != nil {
		t.Fatalf("Search by ID: %v", err)
	}
	if len(results) != 1 || results[0].User.ID != erin || results[0].Relation != RelationNone {
		t.Errorf("按 ID 搜索 = %+v, want erin / none", results)
	}

	// 搜自己的 ID：结果为空
	if results, _ := svc.Search(ctx, alice, "1000000001"); len(results) != 0 {
		t.Errorf("搜自己 = %+v, want 空", results)
	}

	if _, err := svc.Search(ctx, alice, "   "); !errors.Is(err, ErrInvalidKeyword) {
		t.Errorf("空关键字：err = %v, want ErrInvalidKeyword", err)
	}
}

func TestService_ListFriends_Sorted(t *testing.T) {
	svc, _ := newTestService(map[int64]string{
		alice: "me", bob: "bob", carol: "Alice", dave: "alice", erin: "Carl",
	})
	for _, id := range []int64{erin, bob, dave, carol} {
		makeFriends(t, svc, alice, id)
	}

	friends, err := svc.ListFriends(ctx, alice)
	if err != nil {
		t.Fatalf("ListFriends: %v", err)
	}
	// 用户名不区分大小写排序，同名（Alice / alice）按 ID
	want := []int64{carol, dave, bob, erin}
	if len(friends) != len(want) {
		t.Fatalf("got %d friends, want %d", len(friends), len(want))
	}
	for i, f := range friends {
		if f.User.ID != want[i] {
			t.Errorf("friends[%d] = %d (%s), want %d", i, f.User.ID, f.User.Username, want[i])
		}
		if f.Since.IsZero() {
			t.Errorf("friends[%d].Since 为零值", i)
		}
	}
}

func TestService_ListRequests(t *testing.T) {
	svc, _ := newTestService(defaultUsers())
	toBob := mustSend(t, svc, alice, bob)
	mustSend(t, svc, alice, carol)
	mustSend(t, svc, dave, alice)
	if err := svc.Reject(ctx, bob, toBob.ID); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	// 收到的：只有 dave 的待处理申请，Peer 是申请人
	incoming, err := svc.ListIncoming(ctx, alice)
	if err != nil {
		t.Fatalf("ListIncoming: %v", err)
	}
	if len(incoming) != 1 || incoming[0].Peer.ID != dave || incoming[0].Peer.Username != "dave" {
		t.Errorf("incoming = %+v, want 只有 dave", incoming)
	}

	// 发出的：包含被拒绝的申请，申请方离线时靠它看到结果；Peer 是被申请人
	outgoing, err := svc.ListOutgoing(ctx, alice)
	if err != nil {
		t.Fatalf("ListOutgoing: %v", err)
	}
	status := map[int64]RequestStatus{}
	for _, v := range outgoing {
		status[v.Peer.ID] = v.Request.Status
	}
	if len(outgoing) != 2 || status[bob] != StatusRejected || status[carol] != StatusPending {
		t.Errorf("outgoing status = %v, want bob=rejected carol=pending", status)
	}

	// 没有申请时返回空，不报错
	if got, err := svc.ListIncoming(ctx, erin); err != nil || len(got) != 0 {
		t.Errorf("ListIncoming(erin) = %v, %v", got, err)
	}
}
