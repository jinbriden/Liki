package user

import (
	"context"
	"fmt"
	"log/slog"
)

// reviewListLimit 一次最多列出的账号数。测试阶段待审核的账号不会多，超出时命令行会提示
const reviewListLimit = 200

// ReviewService 注册审核（FR-22）。只有服务器上的命令行工具使用，不提供接口和页面：
// 能登录服务器（SSH 密钥）就是管理员身份的证明。
type ReviewService struct {
	repo Repository
}

func NewReviewService(repo Repository) *ReviewService {
	return &ReviewService{repo: repo}
}

// ReviewItem 待审核 / 已拒绝的账号，带上注册时用的邀请码（审核时看备注就知道是给谁的）
type ReviewItem struct {
	User   User
	Invite *InviteCode // 引入邀请码之前的账号为 nil
}

// List 按注册时间先后列出某个状态的账号，最多 reviewListLimit 条
func (s *ReviewService) List(ctx context.Context, status AccountStatus) ([]ReviewItem, error) {
	users, err := s.repo.ListByStatus(ctx, status, reviewListLimit)
	if err != nil {
		return nil, err
	}
	var inviteIDs []int64
	for _, u := range users {
		if u.InviteCodeID != nil {
			inviteIDs = append(inviteIDs, *u.InviteCodeID)
		}
	}
	codes, err := s.repo.FindInvitesByIDs(ctx, inviteIDs)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*InviteCode, len(codes))
	for i := range codes {
		byID[codes[i].ID] = &codes[i]
	}

	items := make([]ReviewItem, 0, len(users))
	for _, u := range users {
		item := ReviewItem{User: u}
		if u.InviteCodeID != nil {
			item.Invite = byID[*u.InviteCodeID]
		}
		items = append(items, item)
	}
	return items, nil
}

// ListLimit 一次最多列出的账号数，命令行据此提示"可能还有更多"
func (s *ReviewService) ListLimit() int { return reviewListLimit }

// Approve 批准账号：待审核、已拒绝的都可以批准（被拒绝后可以再批准）
func (s *ReviewService) Approve(ctx context.Context, userID int64) error {
	return s.transition(ctx, userID, StatusActive, StatusPending, StatusRejected)
}

// Reject 拒绝账号：只能拒绝待审核的。已通过的不能改成拒绝——停用账号需要让已签发的令牌失效，本阶段不做
func (s *ReviewService) Reject(ctx context.Context, userID int64) error {
	return s.transition(ctx, userID, StatusRejected, StatusPending)
}

func (s *ReviewService) transition(ctx context.Context, userID int64, to AccountStatus, from ...AccountStatus) error {
	changed, err := s.repo.UpdateStatus(ctx, userID, to, from...)
	if err != nil {
		return err
	}
	if !changed {
		// 影响 0 行：再查一次，区分"账号不存在"和"当前状态不允许这样转换"，给出明确的提示
		u, err := s.repo.FindByID(ctx, userID)
		if err != nil {
			return err // 包括 ErrUserNotFound
		}
		return fmt.Errorf("%w: account %d is %s", ErrInvalidStatusTransition, userID, u.Status)
	}
	slog.InfoContext(ctx, "account reviewed", "target_user_id", userID, "status", to)
	return nil
}
