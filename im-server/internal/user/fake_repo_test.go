package user

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	u.CreatedAt = time.Now().Truncate(time.Microsecond) // 模拟数据库 now() 回填，精度微秒
	cp := *u                                            // 存副本，避免调用方之后修改 u 影响"库里"的数据
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

func (r *fakeRepo) FindByIDs(ctx context.Context, ids []int64) ([]User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []User
	for _, id := range ids {
		if u, ok := r.users[id]; ok {
			out = append(out, *u)
		}
	}
	return out, nil
}

// SearchByUsernameOrID 用户名只含 ASCII，strings.EqualFold 与 SQL 的 lower() 比较结果一致
func (r *fakeRepo) SearchByUsernameOrID(ctx context.Context, username string, id int64, limit int) ([]User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []User
	for _, u := range r.users {
		if strings.EqualFold(u.Username, username) || u.ID == id {
			out = append(out, *u)
		}
	}
	// 与真实实现一致：ID 命中的排最前，其余按 ID 升序
	slices.SortFunc(out, func(a, b User) int {
		if (a.ID == id) != (b.ID == id) {
			if a.ID == id {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
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
