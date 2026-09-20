-- 账号回收站：删除账号时在同一事务内保存完整行快照，支持后台还原。
-- 还原按原 ID 重建账号并恢复分组关系；回收站条目保留 30 天，可手动永久删除。
CREATE TABLE IF NOT EXISTS accounts_recycle_bin (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL,
    platform TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL DEFAULT '',
    snapshot JSONB NOT NULL,
    group_rows JSONB NOT NULL DEFAULT '[]'::jsonb,
    deleted_by BIGINT NOT NULL DEFAULT 0,
    deleted_by_email TEXT NOT NULL DEFAULT '',
    deleted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_accounts_recycle_bin_deleted_at ON accounts_recycle_bin (deleted_at DESC);
CREATE INDEX IF NOT EXISTS idx_accounts_recycle_bin_account_id ON accounts_recycle_bin (account_id);
