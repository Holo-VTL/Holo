package sqlite

import (
	"context"
	"database/sql"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type AccessPolicyRepo struct {
	db *sql.DB
}

func NewAccessPolicyRepo(db *sql.DB) *AccessPolicyRepo {
	return &AccessPolicyRepo{db: db}
}

func (r *AccessPolicyRepo) Save(ctx context.Context, policy domain.TargetAccessPolicy) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO access_policies (
  policy_id, scope, subject, permission, effective_from, effective_to
) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(policy_id) DO UPDATE SET
  scope=excluded.scope,
  subject=excluded.subject,
  permission=excluded.permission,
  effective_from=excluded.effective_from,
  effective_to=excluded.effective_to`,
		policy.PolicyID,
		string(policy.Scope),
		policy.Subject,
		string(policy.Permission),
		formatTime(policy.EffectiveFrom),
		formatTime(policy.EffectiveTo),
	)
	return err
}

func (r *AccessPolicyRepo) List(ctx context.Context) ([]domain.TargetAccessPolicy, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT policy_id, scope, subject, permission, effective_from, effective_to
FROM access_policies ORDER BY policy_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.TargetAccessPolicy, 0)
	for rows.Next() {
		var policy domain.TargetAccessPolicy
		var scope, permission, effectiveFrom, effectiveTo string
		if err := rows.Scan(&policy.PolicyID, &scope, &policy.Subject, &permission, &effectiveFrom, &effectiveTo); err != nil {
			return nil, err
		}
		policy.Scope = domain.PolicyScope(scope)
		policy.Permission = domain.PolicyPermission(permission)
		policy.EffectiveFrom = parseTime(effectiveFrom)
		policy.EffectiveTo = parseTime(effectiveTo)
		out = append(out, policy)
	}
	return out, rows.Err()
}
