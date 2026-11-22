package friend

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"im-server/internal/user"
)

// fakeRepo 用内存模拟 friend_requests 和 friendships 两张表，并模拟库里的约束：
//   - 外键：to_user_id 不在 userIDs 里 → ErrTargetUserNotFound
//   - uq_friend_requests_pending_pair：两人之间（不分方向）最多一条 pending → ErrPendingExists
//   - Accept / Reject 的条件更新：不是 pending 或不是发给 toUserID 的 → ErrRequestNotPending
type fakeRepo struct {
	mu          sync.Mutex
	userIDs     map[int64]bool
	nextID      int64
	requests    map[int64]*FriendRequest
	friendships map[[2]int64]time.Time // key: {user_id, friend_id}
}

func newFakeRepo(userIDs ...int64) *fakeRepo {
	r := &fakeRepo{
		userIDs:     map[int64]bool{},
		requests:    map[int64]*FriendRequest{},
		friendships: map[[2]int64]time.Time{},
	}
	for _, id := range userIDs {
		r.userIDs[id] = true
	}
	return r
}

func now() time.Time { return time.Now().Truncate(time.Microsecond) } // 模拟数据库 now()，精度微秒

func (r *fakeRepo) pendingBetween(a, b int64) *FriendRequest {
	for _, req := range r.requests {
		if req.Status == StatusPending &&
			((req.FromUserID == a && req.ToUserID == b) || (req.FromUserID == b && req.ToUserID == a)) {
			return req
		}
	}
	return nil
}

func (r *fakeRepo) CreateRequest(ctx context.Context, req *FriendRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.userIDs[req.ToUserID] {
		return ErrTargetUserNotFound
	}
	if r.pendingBetween(req.FromUserID, req.ToUserID) != nil {
		return ErrPendingExists
	}
	r.nextID++
	req.ID = r.nextID
	req.Status = StatusPending
	req.CreatedAt = now()
	cp := *req
	r.requests[req.ID] = &cp
	return nil
}

func (r *fakeRepo) FindPendingBetween(ctx context.Context, a, b int64) (*FriendRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	req := r.pendingBetween(a, b)
	if req == nil {
		return nil, ErrRequestNotFound
	}
	cp := *req
	return &cp, nil
}

func (r *fakeRepo) FindRequestByID(ctx context.Context, id int64) (*FriendRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	req, ok := r.requests[id]
	if !ok {
		return nil, ErrRequestNotFound
	}
	cp := *req
	return &cp, nil
}

// listRequests 返回满足 keep 的申请，按创建时间倒序、同时间按 ID 倒序（和真实实现一致）
func (r *fakeRepo) listRequests(keep func(*FriendRequest) bool) []FriendRequest {
	var out []FriendRequest
	for _, req := range r.requests {
		if keep(req) {
			out = append(out, *req)
		}
	}
	slices.SortFunc(out, func(a, b FriendRequest) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(b.ID, a.ID))
	})
	return out
}

func (r *fakeRepo) ListIncomingPending(ctx context.Context, toUserID int64) ([]FriendRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listRequests(func(req *FriendRequest) bool {
		return req.ToUserID == toUserID && req.Status == StatusPending
	}), nil
}

func (r *fakeRepo) ListOutgoing(ctx context.Context, fromUserID int64, limit int) ([]FriendRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.listRequests(func(req *FriendRequest) bool { return req.FromUserID == fromUserID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *fakeRepo) ListPendingInvolving(ctx context.Context, userID int64, others []int64) ([]FriendRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listRequests(func(req *FriendRequest) bool {
		return req.Status == StatusPending &&
			((req.FromUserID == userID && slices.Contains(others, req.ToUserID)) ||
				(req.ToUserID == userID && slices.Contains(others, req.FromUserID)))
	}), nil
}

// markHandled 模拟条件更新 WHERE id = ? AND to_user_id = ? AND status = 'pending'
func (r *fakeRepo) markHandled(requestID, toUserID int64, status RequestStatus) (*FriendRequest, error) {
	req, ok := r.requests[requestID]
	if !ok || req.ToUserID != toUserID || req.Status != StatusPending {
		return nil, ErrRequestNotPending
	}
	t := now()
	req.Status = status
	req.HandledAt = &t
	return req, nil
}

func (r *fakeRepo) Accept(ctx context.Context, requestID, toUserID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	req, err := r.markHandled(requestID, toUserID, StatusAccepted)
	if err != nil {
		return err
	}
	t := now()
	for _, k := range [][2]int64{{req.FromUserID, req.ToUserID}, {req.ToUserID, req.FromUserID}} {
		if _, ok := r.friendships[k]; !ok { // ON CONFLICT DO NOTHING
			r.friendships[k] = t
		}
	}
	return nil
}

func (r *fakeRepo) Reject(ctx context.Context, requestID, toUserID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.markHandled(requestID, toUserID, StatusRejected)
	return err
}

func (r *fakeRepo) AreFriends(ctx context.Context, a, b int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.friendships[[2]int64{a, b}]
	return ok, nil
}

func (r *fakeRepo) ListFriendships(ctx context.Context, userID int64) ([]Friendship, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Friendship
	for k, t := range r.friendships {
		if k[0] == userID {
			out = append(out, Friendship{UserID: k[0], FriendID: k[1], CreatedAt: t})
		}
	}
	return out, nil
}

func (r *fakeRepo) FilterFriends(ctx context.Context, userID int64, candidates []int64) ([]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []int64
	for _, id := range candidates {
		if _, ok := r.friendships[[2]int64{userID, id}]; ok {
			out = append(out, id)
		}
	}
	return out, nil
}

func (r *fakeRepo) DeleteFriendship(ctx context.Context, a, b int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.friendships, [2]int64{a, b})
	delete(r.friendships, [2]int64{b, a})
	return nil
}

// fakeDirectory 模拟用户模块：按用户名（不区分大小写）或 ID 的字符串形式完全匹配
type fakeDirectory struct {
	profiles []user.Profile
}

func (d *fakeDirectory) GetProfiles(ctx context.Context, ids []int64) ([]user.Profile, error) {
	var out []user.Profile
	for _, p := range d.profiles {
		if slices.Contains(ids, p.ID) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (d *fakeDirectory) SearchProfiles(ctx context.Context, keyword string, limit int) ([]user.Profile, error) {
	var out []user.Profile
	for _, p := range d.profiles {
		if strings.EqualFold(p.Username, keyword) || strconv.FormatInt(p.ID, 10) == keyword {
			out = append(out, p)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// notification fakeNotifier 记录下的一次通知
type notification struct {
	kind   string // received / handled / sync
	userID int64  // 通知给谁
	view   RequestView
}

// fakeNotifier 记录 service 发出的通知，测试据此断言"通知了谁、通知了什么"
type fakeNotifier struct {
	mu     sync.Mutex
	events []notification
}

func (n *fakeNotifier) add(e notification) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, e)
}

func (n *fakeNotifier) RequestReceived(_ context.Context, toUserID int64, view RequestView) {
	n.add(notification{kind: "received", userID: toUserID, view: view})
}

func (n *fakeNotifier) RequestHandled(_ context.Context, fromUserID int64, view RequestView) {
	n.add(notification{kind: "handled", userID: fromUserID, view: view})
}

func (n *fakeNotifier) FriendDataChanged(_ context.Context, userID int64) {
	n.add(notification{kind: "sync", userID: userID})
}

// take 取出并清空已记录的通知
func (n *fakeNotifier) take() []notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := n.events
	n.events = nil
	return out
}

// newTestService 用给定的用户（ID → 用户名）组装 service
func newTestService(users map[int64]string) (*Service, *fakeRepo) {
	svc, repo, _ := newTestServiceWithNotifier(users)
	return svc, repo
}

func newTestServiceWithNotifier(users map[int64]string) (*Service, *fakeRepo, *fakeNotifier) {
	dir := &fakeDirectory{}
	ids := make([]int64, 0, len(users))
	for id, name := range users {
		ids = append(ids, id)
		dir.profiles = append(dir.profiles, user.Profile{ID: id, Username: name})
	}
	slices.SortFunc(dir.profiles, func(a, b user.Profile) int { return cmp.Compare(a.ID, b.ID) })
	repo := newFakeRepo(ids...)
	n := &fakeNotifier{}
	return NewService(repo, dir, n), repo, n
}
