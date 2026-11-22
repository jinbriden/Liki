package friend

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"im-server/internal/user"
)

const (
	maxMessageLen       = 100 // 验证信息最多 100 个字符（按 rune 计数，和库里 VARCHAR(100) 一致）
	maxSearchResults    = 20  // 搜索结果上限
	maxOutgoingRequests = 50  // "我发出的申请"最多返回的条数
)

// UserDirectory 好友模块需要的用户查询能力。
// 按 Go 的习惯由使用方定义接口、只列自己用到的方法；*user.Service 实现了这两个方法，
// 不需要像 Java 那样写 implements 就自动满足（结构化类型）。测试时可以换成 fake。
// 好友模块不直接查 users 表、不跨模块 JOIN，以后拆服务时这里不用改（NFR-07）。
type UserDirectory interface {
	// GetProfiles 批量查用户资料；不存在的 ID 直接跳过，返回顺序不保证。
	GetProfiles(ctx context.Context, ids []int64) ([]user.Profile, error)
	// SearchProfiles 按用户名（不区分大小写，完全匹配）或 10 位用户 ID 搜索，最多 limit 条。
	SearchProfiles(ctx context.Context, keyword string, limit int) ([]user.Profile, error)
}

// SearchResult 搜索结果中的一项
type SearchResult struct {
	User     user.Profile
	Relation Relation
}

// RequestView 申请列表中的一项。Peer 是"对方"：收到的申请里是申请人，发出的申请里是被申请人。
type RequestView struct {
	Request FriendRequest
	Peer    user.Profile
}

// Friend 好友列表中的一项
type Friend struct {
	User  user.Profile
	Since time.Time // 成为好友的时间
}

type Service struct {
	repo  Repository
	users UserDirectory
}

func NewService(repo Repository, users UserDirectory) *Service {
	return &Service{repo: repo, users: users}
}

// Search 按用户名或用户 ID 搜索用户，并标出每个人和 me 的关系（FR-05）。
func (s *Service) Search(ctx context.Context, me int64, keyword string) ([]SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, ErrInvalidKeyword
	}

	// 多查一条：结果里可能有自己，去掉后仍能凑满 maxSearchResults
	profiles, err := s.users.SearchProfiles(ctx, keyword, maxSearchResults+1)
	if err != nil {
		return nil, err
	}
	profiles = slices.DeleteFunc(profiles, func(p user.Profile) bool { return p.ID == me })
	if len(profiles) > maxSearchResults {
		profiles = profiles[:maxSearchResults]
	}
	if len(profiles) == 0 {
		return nil, nil
	}

	ids := make([]int64, 0, len(profiles))
	for _, p := range profiles {
		ids = append(ids, p.ID)
	}

	// 关系一次性批量查出，不对每个人各查一次（避免 N+1 查询）
	relations := make(map[int64]Relation, len(ids))
	pendings, err := s.repo.ListPendingInvolving(ctx, me, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range pendings {
		if r.FromUserID == me {
			relations[r.ToUserID] = RelationPendingOut
		} else {
			relations[r.FromUserID] = RelationPendingIn
		}
	}
	friendIDs, err := s.repo.FilterFriends(ctx, me, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range friendIDs {
		relations[id] = RelationFriend // 好友优先于待处理（极端并发下两者可能同时存在）
	}

	results := make([]SearchResult, 0, len(profiles))
	for _, p := range profiles {
		rel, ok := relations[p.ID]
		if !ok {
			rel = RelationNone
		}
		results = append(results, SearchResult{User: p, Relation: rel})
	}
	return results, nil
}

// SendRequest me 向 toUserID 发送好友申请（FR-06）。
// 同方向已有待处理申请时幂等返回那条已有的申请；对方已申请 me 时返回 ErrReverseRequestPending。
func (s *Service) SendRequest(ctx context.Context, me, toUserID int64, message string) (*FriendRequest, error) {
	if toUserID == me {
		return nil, ErrCannotAddSelf
	}
	message = strings.TrimSpace(message)
	// len 是字节数，一个汉字占 3 字节；要按字符数（rune）计数才和 VARCHAR(100) 一致
	if utf8.RuneCountInString(message) > maxMessageLen {
		return nil, ErrMessageTooLong
	}

	friends, err := s.repo.AreFriends(ctx, me, toUserID)
	if err != nil {
		return nil, err
	}
	if friends {
		return nil, ErrAlreadyFriends
	}

	// 不先查"有没有待处理申请"再插入：查和插之间别人可能插进来（check-then-act 竞态）。
	// 直接插入，由唯一索引判定，冲突了再查是哪一条。
	// 循环两次：冲突后去查时，那条申请可能恰好刚被处理掉，这时再插一次就能成功。
	for range 2 {
		req := &FriendRequest{FromUserID: me, ToUserID: toUserID, Message: message}
		err := s.repo.CreateRequest(ctx, req)
		if err == nil {
			return req, nil
		}
		if !errors.Is(err, ErrPendingExists) {
			return nil, err
		}

		existing, err := s.repo.FindPendingBetween(ctx, me, toUserID)
		if errors.Is(err, ErrRequestNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if existing.FromUserID == me {
			return existing, nil // 同方向重复申请：幂等返回已有申请
		}
		return nil, ErrReverseRequestPending
	}
	return nil, errors.New("send friend request: pending request changed concurrently, please retry")
}

// ListIncoming 我收到的待处理申请（FR-07、FR-09）。
func (s *Service) ListIncoming(ctx context.Context, me int64) ([]RequestView, error) {
	reqs, err := s.repo.ListIncomingPending(ctx, me)
	if err != nil {
		return nil, err
	}
	return s.withPeers(ctx, reqs, func(r FriendRequest) int64 { return r.FromUserID })
}

// ListOutgoing 我发出的申请（含处理结果），最多 maxOutgoingRequests 条。
func (s *Service) ListOutgoing(ctx context.Context, me int64) ([]RequestView, error) {
	reqs, err := s.repo.ListOutgoing(ctx, me, maxOutgoingRequests)
	if err != nil {
		return nil, err
	}
	return s.withPeers(ctx, reqs, func(r FriendRequest) int64 { return r.ToUserID })
}

// withPeers 用一次 GetProfiles 批量补齐每条申请"对方"的资料，保持 reqs 原有顺序
func (s *Service) withPeers(ctx context.Context, reqs []FriendRequest, peerID func(FriendRequest) int64) ([]RequestView, error) {
	ids := make([]int64, 0, len(reqs))
	for _, r := range reqs {
		ids = append(ids, peerID(r))
	}
	profiles, err := s.profileMap(ctx, ids)
	if err != nil {
		return nil, err
	}

	views := make([]RequestView, 0, len(reqs))
	for _, r := range reqs {
		views = append(views, RequestView{Request: r, Peer: profiles.get(peerID(r))})
	}
	return views, nil
}

// Accept me 同意申请 requestID，双方成为好友（FR-07）。
// M4：成功后实时通知申请方。
func (s *Service) Accept(ctx context.Context, me, requestID int64) error {
	return s.handle(ctx, me, requestID, s.repo.Accept)
}

// Reject me 拒绝申请 requestID（FR-07）。
// M4：成功后实时通知申请方。
func (s *Service) Reject(ctx context.Context, me, requestID int64) error {
	return s.handle(ctx, me, requestID, s.repo.Reject)
}

// handle Accept 和 Reject 的公共流程。do 传的是方法值（s.repo.Accept），
// 已绑定接收者，可以像普通函数一样调用，类似 Java 的方法引用 repo::accept。
func (s *Service) handle(ctx context.Context, me, requestID int64, do func(ctx context.Context, requestID, toUserID int64) error) error {
	req, err := s.repo.FindRequestByID(ctx, requestID)
	if err != nil {
		return err
	}
	// 不是发给我的申请，和不存在一样处理，不暴露别人的申请
	if req.ToUserID != me {
		return ErrRequestNotFound
	}
	// 先查一次是为了给出准确的错误；真正防并发的是 repository 里的条件更新
	if req.Status != StatusPending {
		return ErrRequestNotPending
	}
	return do(ctx, requestID, me)
}

// ListFriends 我的好友列表，按用户名排序（不区分大小写）、同名再按 ID 排序（FR-09）。
func (s *Service) ListFriends(ctx context.Context, me int64) ([]Friend, error) {
	fs, err := s.repo.ListFriendships(ctx, me)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(fs))
	for _, f := range fs {
		ids = append(ids, f.FriendID)
	}
	profiles, err := s.profileMap(ctx, ids)
	if err != nil {
		return nil, err
	}

	friends := make([]Friend, 0, len(fs))
	for _, f := range fs {
		friends = append(friends, Friend{User: profiles.get(f.FriendID), Since: f.CreatedAt})
	}
	slices.SortFunc(friends, func(a, b Friend) int {
		// cmp.Or 返回第一个非 0 的结果：用户名相同再比较 ID
		return cmp.Or(
			strings.Compare(strings.ToLower(a.User.Username), strings.ToLower(b.User.Username)),
			cmp.Compare(a.User.ID, b.User.ID),
		)
	})
	return friends, nil
}

// DeleteFriend 双向解除好友关系，不通知对方（FR-08）。本来就不是好友时也返回 nil（幂等）。
func (s *Service) DeleteFriend(ctx context.Context, me, friendID int64) error {
	return s.repo.DeleteFriendship(ctx, me, friendID)
}

// AreFriends 两人是否是好友。供 M4 的消息模块检查非好友限制（FR-12）。
func (s *Service) AreFriends(ctx context.Context, a, b int64) (bool, error) {
	return s.repo.AreFriends(ctx, a, b)
}

// profiles 按用户 ID 索引的用户资料
type profiles map[int64]user.Profile

// get 查不到时（理论上外键保证不会发生）只返回 ID，不让整个列表失败
func (m profiles) get(id int64) user.Profile {
	if p, ok := m[id]; ok {
		return p
	}
	return user.Profile{ID: id}
}

func (s *Service) profileMap(ctx context.Context, ids []int64) (profiles, error) {
	if len(ids) == 0 {
		return profiles{}, nil
	}
	list, err := s.users.GetProfiles(ctx, ids)
	if err != nil {
		return nil, err
	}
	m := make(profiles, len(list))
	for _, p := range list {
		m[p.ID] = p
	}
	return m, nil
}
