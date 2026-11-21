package user

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// fakeRepo 用 map 模拟 users 表：ID 是主键（重复时返回 ErrDuplicateID），用户名可以重复
type fakeRepo struct {
	mu    sync.Mutex
	users map[int64]*User
}

func newFakeRepo() *fakeRepo { return &fakeRepo{users: map[int64]*User{}} }

func (r *fakeRepo) Create(ctx context.Context, u *User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.users[u.ID]; ok {
		return ErrDuplicateID
	}
	u.CreatedAt = time.Now()
	cp := *u // 存副本，避免调用方之后修改 u 影响"库里"的数据
	r.users[u.ID] = &cp
	return nil
}

func (r *fakeRepo) FindByID(ctx context.Context, id int64) (*User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}

// counterIDs 从 1000000001 开始依次递增生成 ID
func counterIDs() IDGenerator {
	var mu sync.Mutex
	next := int64(minUserID)
	return func() (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		next++
		return next, nil
	}
}

// fixedIDs 按顺序返回给定的 ID，用完后返回错误
func fixedIDs(ids ...int64) IDGenerator {
	i := 0
	return func() (int64, error) {
		if i >= len(ids) {
			return 0, errors.New("fixedIDs: exhausted")
		}
		i++
		return ids[i-1], nil
	}
}

// fakeTokens 返回可预测的假令牌；err 不为空时模拟签发失败
type fakeTokens struct{ err error }

func (f fakeTokens) Issue(userID int64) (string, time.Time, error) {
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	return fmt.Sprintf("token-for-%d", userID), time.Unix(1_800_000_000, 0), nil
}

func newTestService() (*Service, *fakeRepo) {
	repo := newFakeRepo()
	return NewService(repo, fakeTokens{}, counterIDs()), repo
}
