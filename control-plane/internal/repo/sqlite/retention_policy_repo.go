package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type RetentionPolicyRepo struct {
	db *sql.DB
}

func NewRetentionPolicyRepo(db *sql.DB) *RetentionPolicyRepo {
	return &RetentionPolicyRepo{db: db}
}

func (r *RetentionPolicyRepo) Save(ctx context.Context, policy domain.RetentionPolicy) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO retention_policies (
  retention_id, cartridge_id, mode, lock_until, created_by
) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(retention_id) DO UPDATE SET
  cartridge_id=excluded.cartridge_id,
  mode=excluded.mode,
  lock_until=excluded.lock_until,
  created_by=excluded.created_by`,
		policy.RetentionID,
		policy.CartridgeID,
		string(policy.Mode),
		formatTime(policy.LockUntil),
		policy.CreatedBy,
	)
	return err
}

func (r *RetentionPolicyRepo) Find(ctx context.Context, retentionID string) (domain.RetentionPolicy, error) {
	var policy domain.RetentionPolicy
	var mode, lockUntil string
	err := r.db.QueryRowContext(ctx, `
SELECT retention_id, cartridge_id, mode, lock_until, created_by
FROM retention_policies WHERE retention_id = ?`, retentionID).Scan(
		&policy.RetentionID,
		&policy.CartridgeID,
		&mode,
		&lockUntil,
		&policy.CreatedBy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RetentionPolicy{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.RetentionPolicy{}, err
	}
	policy.Mode = domain.RetentionMode(mode)
	policy.LockUntil = parseTime(lockUntil)
	return policy, nil
}
