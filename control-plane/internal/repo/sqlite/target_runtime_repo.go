package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"

	sqlitedriver "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const maxSQLiteValidationMediaEntries = 1000
const maxSQLiteValidationMediaAge = time.Hour

type TargetRuntimeRepo struct {
	db                     *sql.DB
	mediaMu                sync.RWMutex
	validationMedia        map[string][]byte
	validationMediaWritten map[string]time.Time
	validationMediaOrder   []string
}

func NewTargetRuntimeRepo(db *sql.DB) *TargetRuntimeRepo {
	return &TargetRuntimeRepo{
		db:                     db,
		validationMedia:        make(map[string][]byte),
		validationMediaWritten: make(map[string]time.Time),
		validationMediaOrder:   make([]string, 0, maxSQLiteValidationMediaEntries),
	}
}

func (r *TargetRuntimeRepo) SavePublication(ctx context.Context, p *domain.TargetPublication) error {
	err := savePublicationTx(ctx, r.db, p)
	if isConstraintError(err) {
		return domain.ErrConflict
	}
	return err
}

type publicationExecContext interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func savePublicationTx(ctx context.Context, execer publicationExecContext, p *domain.TargetPublication) error {
	_, err := execer.ExecContext(ctx, `
INSERT INTO target_publications (
  publication_id, pool_id, library_id, drive_id, cartridge_id, target_iqn,
  device_role, device_profile, drive_profile, portal, state, last_error,
  compression_enabled, dedup_enabled, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(publication_id) DO UPDATE SET
  pool_id=excluded.pool_id,
  library_id=excluded.library_id,
  drive_id=excluded.drive_id,
  cartridge_id=excluded.cartridge_id,
  target_iqn=excluded.target_iqn,
  device_role=excluded.device_role,
  device_profile=excluded.device_profile,
  drive_profile=excluded.drive_profile,
  portal=excluded.portal,
  state=excluded.state,
  last_error=excluded.last_error,
  compression_enabled=excluded.compression_enabled,
  dedup_enabled=excluded.dedup_enabled,
  updated_at=excluded.updated_at`,
		p.PublicationID,
		p.PoolID,
		p.LibraryID,
		p.DriveID,
		p.CartridgeID,
		p.TargetIQN,
		p.DeviceRole,
		p.DeviceProfile,
		p.DriveProfile,
		p.Portal,
		string(p.State),
		p.LastError,
		boolToInt(p.CompressionEnabled),
		boolToInt(p.DedupEnabled),
		formatTime(p.CreatedAt),
		formatTime(p.UpdatedAt),
	)
	return err
}

func (r *TargetRuntimeRepo) SavePublicationIfIQNAvailable(ctx context.Context, p *domain.TargetPublication) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var existingID string
	err = tx.QueryRowContext(ctx, `
SELECT publication_id
FROM target_publications
WHERE target_iqn = ? AND state IN ('creating', 'ready') AND publication_id <> ?
LIMIT 1`, p.TargetIQN, p.PublicationID).Scan(&existingID)
	if err == nil {
		_, _ = tx.ExecContext(ctx, `
UPDATE target_publications
SET state = 'failed', last_error = 'superseded by retry', updated_at = ?
WHERE publication_id = ? AND state = 'creating'`, formatTime(time.Now().UTC()), existingID)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return err
	}
	if err := savePublicationTx(ctx, tx, p); err != nil {
		_ = tx.Rollback()
		if isConstraintError(err) {
			return domain.ErrConflict
		}
		return err
	}
	return tx.Commit()
}

func (r *TargetRuntimeRepo) FindPublication(ctx context.Context, publicationID string) (*domain.TargetPublication, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT publication_id, pool_id, library_id, drive_id, cartridge_id, target_iqn,
       device_role, device_profile, drive_profile, portal, state, last_error,
       compression_enabled, dedup_enabled, created_at, updated_at
FROM target_publications
WHERE publication_id = ?`, publicationID)
	return scanPublication(row)
}

func (r *TargetRuntimeRepo) ListPublications(ctx context.Context) []*domain.TargetPublication {
	rows, err := r.db.QueryContext(ctx, `
SELECT publication_id, pool_id, library_id, drive_id, cartridge_id, target_iqn,
       device_role, device_profile, drive_profile, portal, state, last_error,
       compression_enabled, dedup_enabled, created_at, updated_at
FROM target_publications
ORDER BY publication_id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanPublications(rows)
}

func (r *TargetRuntimeRepo) ListDiscoverablePublications(ctx context.Context) []*domain.TargetPublication {
	rows, err := r.db.QueryContext(ctx, `
SELECT publication_id, pool_id, library_id, drive_id, cartridge_id, target_iqn,
       device_role, device_profile, drive_profile, portal, state, last_error,
       compression_enabled, dedup_enabled, created_at, updated_at
FROM target_publications
WHERE state = 'ready' AND target_iqn <> '' AND portal <> ''
ORDER BY publication_id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanPublications(rows)
}

func (r *TargetRuntimeRepo) FindPublicationByIQN(ctx context.Context, iqn string) (*domain.TargetPublication, bool) {
	row := r.db.QueryRowContext(ctx, `
SELECT publication_id, pool_id, library_id, drive_id, cartridge_id, target_iqn,
       device_role, device_profile, drive_profile, portal, state, last_error,
       compression_enabled, dedup_enabled, created_at, updated_at
FROM target_publications
WHERE target_iqn = ? AND state IN ('creating', 'ready')
ORDER BY publication_id
LIMIT 1`, iqn)
	p, err := scanPublication(row)
	if err != nil {
		return nil, false
	}
	return p, true
}

func (r *TargetRuntimeRepo) SaveValidationRun(ctx context.Context, run *domain.ValidationRun) error {
	finishedAt := ""
	if run.FinishedAt != nil {
		finishedAt = formatTime(*run.FinishedAt)
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO validation_runs (
  validation_id, publication_id, scenario, status, mode, bytes_written, bytes_read,
  write_digest, read_digest, evidence_path, started_at, finished_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(validation_id) DO UPDATE SET
  publication_id=excluded.publication_id,
  scenario=excluded.scenario,
  status=excluded.status,
  mode=excluded.mode,
  bytes_written=excluded.bytes_written,
  bytes_read=excluded.bytes_read,
  write_digest=excluded.write_digest,
  read_digest=excluded.read_digest,
  evidence_path=excluded.evidence_path,
  started_at=excluded.started_at,
  finished_at=excluded.finished_at`,
		run.ValidationID,
		run.PublicationID,
		string(run.Scenario),
		string(run.Status),
		string(run.Mode),
		run.BytesWritten,
		run.BytesRead,
		run.WriteDigest,
		run.ReadDigest,
		run.EvidencePath,
		formatTime(run.StartedAt),
		finishedAt,
	)
	if isConstraintError(err) {
		return domain.ErrConflict
	}
	return err
}

func (r *TargetRuntimeRepo) WriteValidationMedia(ctx context.Context, publicationID string, payload []byte) error {
	if _, err := r.FindPublication(ctx, publicationID); err != nil {
		return err
	}
	r.mediaMu.Lock()
	defer r.mediaMu.Unlock()
	cp := make([]byte, len(payload))
	copy(cp, payload)
	if _, exists := r.validationMedia[publicationID]; !exists {
		r.validationMediaOrder = append(r.validationMediaOrder, publicationID)
	}
	r.validationMedia[publicationID] = cp
	now := time.Now().UTC()
	r.validationMediaWritten[publicationID] = now
	r.evictOldValidationMedia(now, maxSQLiteValidationMediaAge)
	r.evictValidationMediaOverflow()
	return nil
}

func (r *TargetRuntimeRepo) ReadValidationMedia(ctx context.Context, publicationID string) ([]byte, error) {
	if _, err := r.FindPublication(ctx, publicationID); err != nil {
		return nil, err
	}
	r.mediaMu.RLock()
	defer r.mediaMu.RUnlock()
	payload := r.validationMedia[publicationID]
	cp := make([]byte, len(payload))
	copy(cp, payload)
	return cp, nil
}

func (r *TargetRuntimeRepo) ListValidationRuns(ctx context.Context, publicationID string) []*domain.ValidationRun {
	rows, err := r.db.QueryContext(ctx, `
SELECT validation_id, publication_id, scenario, status, mode, bytes_written, bytes_read,
       write_digest, read_digest, evidence_path, started_at, finished_at
FROM validation_runs
WHERE publication_id = ?
ORDER BY started_at, validation_id`, publicationID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]*domain.ValidationRun, 0)
	for rows.Next() {
		run, err := scanValidationRun(rows)
		if err != nil {
			return nil
		}
		out = append(out, run)
	}
	return out
}

func (r *TargetRuntimeRepo) evictOldValidationMedia(now time.Time, maxAge time.Duration) {
	cutoff := now.Add(-maxAge)
	keptOrder := make([]string, 0, len(r.validationMediaOrder))
	for _, publicationID := range r.validationMediaOrder {
		writtenAt, ok := r.validationMediaWritten[publicationID]
		if !ok || writtenAt.Before(cutoff) {
			delete(r.validationMedia, publicationID)
			delete(r.validationMediaWritten, publicationID)
			continue
		}
		keptOrder = append(keptOrder, publicationID)
	}
	r.validationMediaOrder = keptOrder
}

func (r *TargetRuntimeRepo) evictValidationMediaOverflow() {
	for len(r.validationMediaOrder) > maxSQLiteValidationMediaEntries {
		evictID := r.validationMediaOrder[0]
		r.validationMediaOrder = r.validationMediaOrder[1:]
		delete(r.validationMedia, evictID)
		delete(r.validationMediaWritten, evictID)
	}
}

type publicationScanner interface {
	Scan(dest ...any) error
}

func scanPublication(row publicationScanner) (*domain.TargetPublication, error) {
	var (
		p                    domain.TargetPublication
		state                string
		createdAt, updatedAt string
		compressionEnabled   int
		dedupEnabled         int
	)
	err := row.Scan(
		&p.PublicationID,
		&p.PoolID,
		&p.LibraryID,
		&p.DriveID,
		&p.CartridgeID,
		&p.TargetIQN,
		&p.DeviceRole,
		&p.DeviceProfile,
		&p.DriveProfile,
		&p.Portal,
		&state,
		&p.LastError,
		&compressionEnabled,
		&dedupEnabled,
		&createdAt,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.State = domain.PublicationState(state)
	p.CompressionEnabled = compressionEnabled != 0
	p.DedupEnabled = dedupEnabled != 0
	p.CreatedAt = parseTime(createdAt)
	p.UpdatedAt = parseTime(updatedAt)
	return &p, nil
}

func scanPublications(rows *sql.Rows) []*domain.TargetPublication {
	out := make([]*domain.TargetPublication, 0)
	for rows.Next() {
		p, err := scanPublication(rows)
		if err != nil {
			return nil
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PublicationID < out[j].PublicationID
	})
	return out
}

func scanValidationRun(rows *sql.Rows) (*domain.ValidationRun, error) {
	var (
		run        domain.ValidationRun
		scenario   string
		status     string
		mode       string
		startedAt  string
		finishedAt string
	)
	if err := rows.Scan(
		&run.ValidationID,
		&run.PublicationID,
		&scenario,
		&status,
		&mode,
		&run.BytesWritten,
		&run.BytesRead,
		&run.WriteDigest,
		&run.ReadDigest,
		&run.EvidencePath,
		&startedAt,
		&finishedAt,
	); err != nil {
		return nil, err
	}
	run.Scenario = domain.ValidationScenario(scenario)
	run.Status = domain.ValidationStatus(status)
	run.Mode = domain.ValidationMode(mode)
	run.StartedAt = parseTime(startedAt)
	if parsed := parseTime(finishedAt); !parsed.IsZero() {
		run.FinishedAt = &parsed
	}
	return &run, nil
}

func isConstraintError(err error) bool {
	var sqliteErr *sqlitedriver.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() {
	case sqlite3.SQLITE_CONSTRAINT,
		sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY,
		sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY,
		sqlite3.SQLITE_CONSTRAINT_UNIQUE:
		return true
	default:
		return false
	}
}
