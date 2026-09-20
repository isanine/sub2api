package service

import (
	"context"
	"time"
)

// AccountRecycleBinEntry 是回收站条目摘要（不含完整快照与凭证材料）。
type AccountRecycleBinEntry struct {
	ID             int64     `json:"id"`
	AccountID      int64     `json:"account_id"`
	Platform       string    `json:"platform"`
	Name           string    `json:"name"`
	Type           string    `json:"type"`
	GroupIDs       []int64   `json:"group_ids"`
	DeletedBy      int64     `json:"deleted_by"`
	DeletedByEmail   string     `json:"deleted_by_email"`
	DeletedAt        time.Time  `json:"deleted_at"`
	AccountExpiresAt *time.Time `json:"account_expires_at"` // 账号订阅到期时间（删除时快照；nil = 永久）
}

// accountRecycleOperatorContextKey 携带删除操作人（管理端用户），由 handler 注入、
// repository 在快照落库时读取。
type accountRecycleOperatorContextKey struct{}

// AccountRecycleOperator 描述一次账号删除的操作人。
type AccountRecycleOperator struct {
	UserID int64
	Email  string
}

// ContextWithAccountRecycleOperator 在 ctx 上附加回收站操作人信息。
func ContextWithAccountRecycleOperator(ctx context.Context, op AccountRecycleOperator) context.Context {
	return context.WithValue(ctx, accountRecycleOperatorContextKey{}, op)
}

// AccountRecycleOperatorFromContext 读取回收站操作人；未设置时返回零值。
func AccountRecycleOperatorFromContext(ctx context.Context) AccountRecycleOperator {
	if ctx == nil {
		return AccountRecycleOperator{}
	}
	if op, ok := ctx.Value(accountRecycleOperatorContextKey{}).(AccountRecycleOperator); ok {
		return op
	}
	return AccountRecycleOperator{}
}

// AccountRecycleBinRetention 保留天数：超过后由后续删除动作机会式清理。
const AccountRecycleBinRetentionDays = 30

// ListAccountRecycleBin 列出回收站（最近删除优先）。
func (s *adminServiceImpl) ListAccountRecycleBin(ctx context.Context, limit int) ([]AccountRecycleBinEntry, error) {
	if s == nil || s.accountRepo == nil {
		return nil, nil
	}
	return s.accountRepo.ListAccountRecycleBin(ctx, limit)
}

// RestoreAccountFromRecycleBin 从回收站还原账号：按原 ID 重建行、恢复分组关系，
// 并删除回收站条目。调度器通过 outbox 事件感知账号变更。
func (s *adminServiceImpl) RestoreAccountFromRecycleBin(ctx context.Context, binID int64) (*Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, ErrAccountNotFound
	}
	return s.accountRepo.RestoreAccountFromRecycleBin(ctx, binID)
}

// PurgeAccountRecycleBinEntry 永久删除回收站条目。
func (s *adminServiceImpl) PurgeAccountRecycleBinEntry(ctx context.Context, binID int64) error {
	if s == nil || s.accountRepo == nil {
		return ErrAccountNotFound
	}
	return s.accountRepo.PurgeAccountRecycleBinEntry(ctx, binID)
}
