package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo"
)

const maxISCSISecurityHistory = 20

var _ repo.ISCSISecurityRepository = (*ISCSISecurityRepo)(nil)

type ISCSISecurityRepo struct {
	db *sql.DB
}

func NewISCSISecurityRepo(db *sql.DB) *ISCSISecurityRepo {
	return &ISCSISecurityRepo{db: db}
}

func (r *ISCSISecurityRepo) SaveBinding(ctx context.Context, binding domain.ISCSISecurityBinding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	table, keyColumn, key := securityBindingTable(binding)
	if table == "" {
		return domain.ErrInvalidInput
	}
	exists, generation, err := readBindingGeneration(ctx, tx, table, keyColumn, key)
	if err != nil {
		return err
	}
	if binding.Scope == domain.SecurityScopeTarget && exists {
		var libraryID, driveID, role string
		err := tx.QueryRowContext(ctx, `SELECT library_id, COALESCE(drive_id,''), device_role FROM target_iscsi_security WHERE target_iqn=?`, key).Scan(&libraryID, &driveID, &role)
		if err != nil || libraryID != binding.LibraryID || driveID != binding.DriveID || role != binding.DeviceRole {
			return domain.ErrConflict
		}
	}
	if binding.Scope == domain.SecurityScopeTarget && !exists {
		return domain.ErrNotFound
	}
	if (!exists && binding.Generation != 1) || (exists && binding.Generation != generation+1) {
		return domain.ErrConflict
	}
	initiators := []string{}
	if binding.Authentication != nil {
		initiators = append(initiators, binding.Authentication.Initiators...)
	}
	sort.Strings(initiators)
	initiatorsJSON, err := json.Marshal(initiators)
	if err != nil {
		return domain.ErrInvalidInput
	}
	var authMode, credentialID any
	restrictInitiators := 0
	if binding.Authentication != nil {
		authMode = string(binding.Authentication.Mode)
		if binding.Authentication.RestrictInitiators {
			restrictInitiators = 1
		}
		if binding.Authentication.CredentialID != "" {
			credentialID = binding.Authentication.CredentialID
		}
	}
	var result sql.Result
	if exists {
		if binding.Scope == domain.SecurityScopeDrive {
			result, err = tx.ExecContext(ctx, `UPDATE drive_iscsi_security SET auth_mode=?, credential_id=?, initiators_json=?, restrict_initiators=?, generation=? WHERE drive_id=? AND generation=?`, authMode, credentialID, string(initiatorsJSON), restrictInitiators, binding.Generation, binding.OwnerID, generation)
		} else if binding.Scope == domain.SecurityScopeTarget {
			result, err = tx.ExecContext(ctx, `UPDATE target_iscsi_security SET auth_mode=?, credential_id=?, initiators_json=?, restrict_initiators=?, generation=? WHERE target_iqn=? AND generation=?`, authMode, credentialID, string(initiatorsJSON), restrictInitiators, binding.Generation, key, generation)
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE `+table+` SET auth_mode=?, credential_id=?, initiators_json=?, restrict_initiators=?, generation=? WHERE `+keyColumn+`=? AND generation=?`, authMode, credentialID, string(initiatorsJSON), restrictInitiators, binding.Generation, key, generation)
		}
	} else {
		switch binding.Scope {
		case domain.SecurityScopeLibrary:
			result, err = tx.ExecContext(ctx, `INSERT INTO library_iscsi_security(library_id, auth_mode, credential_id, initiators_json, restrict_initiators, generation) VALUES(?, ?, ?, ?, ?, ?)`, binding.OwnerID, authMode, credentialID, string(initiatorsJSON), restrictInitiators, binding.Generation)
		case domain.SecurityScopeDrive:
			result, err = tx.ExecContext(ctx, `INSERT INTO drive_iscsi_security(drive_id, library_id, auth_mode, credential_id, initiators_json, restrict_initiators, generation) VALUES(?, ?, ?, ?, ?, ?, ?)`, binding.OwnerID, binding.LibraryID, authMode, credentialID, string(initiatorsJSON), restrictInitiators, binding.Generation)
		}
	}
	if err != nil {
		return mapSecurityRepoError(err)
	}
	if result != nil {
		if n, rowsErr := result.RowsAffected(); rowsErr != nil || n != 1 {
			return domain.ErrConflict
		}
	}
	return mapSecurityRepoError(tx.Commit())
}

func (r *ISCSISecurityRepo) FindBinding(ctx context.Context, scope domain.SecurityScope, ownerID string) (domain.ISCSISecurityBinding, error) {
	table, keyColumn, _ := securityBindingTable(domain.ISCSISecurityBinding{Scope: scope})
	if table == "" || ownerID == "" {
		return domain.ISCSISecurityBinding{}, domain.ErrInvalidInput
	}
	query := `SELECT ` + keyColumn + `, auth_mode, credential_id, initiators_json, restrict_initiators, generation FROM ` + table + ` WHERE ` + keyColumn + `=?`
	if scope == domain.SecurityScopeDrive {
		query = `SELECT drive_id, library_id, auth_mode, credential_id, initiators_json, restrict_initiators, generation FROM drive_iscsi_security WHERE drive_id=?`
		var binding domain.ISCSISecurityBinding
		var authMode, credentialID sql.NullString
		var initiatorsJSON string
		var restrictInitiators int
		err := r.db.QueryRowContext(ctx, query, ownerID).Scan(&binding.OwnerID, &binding.LibraryID, &authMode, &credentialID, &initiatorsJSON, &restrictInitiators, &binding.Generation)
		if err != nil {
			return domain.ISCSISecurityBinding{}, mapSecurityRepoError(err)
		}
		binding.Scope = scope
		if err := fillBindingPolicies(&binding, authMode, credentialID, initiatorsJSON, restrictInitiators); err != nil {
			return domain.ISCSISecurityBinding{}, err
		}
		return binding, nil
	}
	var binding domain.ISCSISecurityBinding
	var authMode, credentialID sql.NullString
	var initiatorsJSON string
	var restrictInitiators int
	if scope == domain.SecurityScopeTarget {
		query = `SELECT target_iqn, library_id, COALESCE(drive_id,''), device_role, administrative_offline, auth_mode, credential_id, initiators_json, restrict_initiators, generation FROM target_iscsi_security WHERE target_iqn=?`
		var offline int
		err := r.db.QueryRowContext(ctx, query, ownerID).Scan(&binding.TargetIQN, &binding.LibraryID, &binding.DriveID, &binding.DeviceRole, &offline, &authMode, &credentialID, &initiatorsJSON, &restrictInitiators, &binding.Generation)
		if err != nil {
			return domain.ISCSISecurityBinding{}, mapSecurityRepoError(err)
		}
		binding.AdministrativeOffline = offline != 0
	} else {
		err := r.db.QueryRowContext(ctx, query, ownerID).Scan(&binding.OwnerID, &authMode, &credentialID, &initiatorsJSON, &restrictInitiators, &binding.Generation)
		if err != nil {
			return domain.ISCSISecurityBinding{}, mapSecurityRepoError(err)
		}
	}
	binding.Scope = scope
	if scope == domain.SecurityScopeLibrary {
		binding.LibraryID = ""
	}
	if scope == domain.SecurityScopeTarget {
		binding.OwnerID = binding.TargetIQN
	}
	if err := fillBindingPolicies(&binding, authMode, credentialID, initiatorsJSON, restrictInitiators); err != nil {
		return domain.ISCSISecurityBinding{}, err
	}
	return binding, nil
}

func fillBindingPolicies(binding *domain.ISCSISecurityBinding, authMode, credentialID sql.NullString, initiatorsJSON string, restrictInitiators int) error {
	if authMode.Valid {
		var initiators []string
		if err := json.Unmarshal([]byte(initiatorsJSON), &initiators); err != nil {
			return domain.ErrInvalidState
		}
		binding.Authentication = &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthMode(authMode.String), Initiators: initiators, RestrictInitiators: restrictInitiators != 0}
		if credentialID.Valid {
			binding.Authentication.CredentialID = credentialID.String
		}
	}
	return nil
}

func (r *ISCSISecurityRepo) RegisterTarget(ctx context.Context, targetIQN, libraryID, driveID, role string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := registerTargetTx(ctx, tx, targetIQN, libraryID, driveID, role, false); err != nil {
		return err
	}
	return mapSecurityRepoError(tx.Commit())
}

func registerTargetTx(ctx context.Context, tx *sql.Tx, targetIQN, libraryID, driveID, role string, offline bool) error {
	if domain.ValidateTargetIQN(targetIQN) != nil || domain.ValidateManagementID(libraryID) != nil {
		return domain.ErrInvalidInput
	}
	if role == "changer" && driveID != "" || role == "drive" && domain.ValidateManagementID(driveID) != nil || role != "changer" && role != "drive" {
		return domain.ErrInvalidInput
	}
	var oldLibrary, oldDrive, oldRole string
	err := tx.QueryRowContext(ctx, `SELECT library_id, COALESCE(drive_id,''), device_role FROM target_iscsi_security WHERE target_iqn=?`, targetIQN).Scan(&oldLibrary, &oldDrive, &oldRole)
	if err == nil {
		if oldLibrary != libraryID || oldDrive != driveID || oldRole != role {
			return domain.ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO target_iscsi_security(target_iqn, library_id, drive_id, device_role, administrative_offline, initiators_json, generation) VALUES(?, ?, ?, ?, ?, '[]', 1)`, targetIQN, libraryID, nullableDriveID(driveID), role, boolToInt(offline))
	return mapSecurityRepoError(err)
}

func nullableDriveID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

func (r *ISCSISecurityRepo) SetTargetOffline(ctx context.Context, targetIQN string, offline bool) error {
	result, err := r.db.ExecContext(ctx, `UPDATE target_iscsi_security SET administrative_offline=? WHERE target_iqn=?`, boolToInt(offline), targetIQN)
	if err != nil {
		return mapSecurityRepoError(err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *ISCSISecurityRepo) FindTarget(ctx context.Context, targetIQN string) (domain.ISCSISecurityBinding, error) {
	return r.FindBinding(ctx, domain.SecurityScopeTarget, targetIQN)
}

func (r *ISCSISecurityRepo) ListTargets(ctx context.Context) ([]domain.ISCSISecurityBinding, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT target_iqn, library_id, COALESCE(drive_id,''), device_role, administrative_offline, auth_mode, credential_id, initiators_json, restrict_initiators, generation FROM target_iscsi_security ORDER BY target_iqn")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.ISCSISecurityBinding, 0)
	for rows.Next() {
		var binding domain.ISCSISecurityBinding
		var offline int
		var authMode, credentialID sql.NullString
		var initiatorsJSON string
		var restrictInitiators int
		if err := rows.Scan(&binding.TargetIQN, &binding.LibraryID, &binding.DriveID, &binding.DeviceRole, &offline, &authMode, &credentialID, &initiatorsJSON, &restrictInitiators, &binding.Generation); err != nil {
			return nil, err
		}
		binding.Scope = domain.SecurityScopeTarget
		binding.OwnerID = binding.TargetIQN
		binding.AdministrativeOffline = offline != 0
		if err := fillBindingPolicies(&binding, authMode, credentialID, initiatorsJSON, restrictInitiators); err != nil {
			return nil, err
		}
		out = append(out, binding)
	}
	return out, rows.Err()
}

func (r *ISCSISecurityRepo) CreateCredential(ctx context.Context, credential domain.ISCSICredential) error {
	if credential.Version != 1 || credential.Validate() != nil {
		return domain.ErrInvalidInput
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO iscsi_chap_credentials(credential_id, label, username, mutual_username, encrypted_secret, version, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		credential.CredentialID, credential.Label, credential.Username, credential.MutualUsername, credential.EncryptedSecret, credential.Version, formatTime(credential.CreatedAt))
	return mapSecurityRepoError(err)
}

func (r *ISCSISecurityRepo) FindCredential(ctx context.Context, credentialID string) (domain.ISCSICredential, error) {
	var credential domain.ISCSICredential
	var created string
	err := r.db.QueryRowContext(ctx, `SELECT credential_id, label, username, mutual_username, encrypted_secret, version, created_at FROM iscsi_chap_credentials WHERE credential_id=?`, credentialID).Scan(&credential.CredentialID, &credential.Label, &credential.Username, &credential.MutualUsername, &credential.EncryptedSecret, &credential.Version, &created)
	if err != nil {
		return domain.ISCSICredential{}, mapSecurityRepoError(err)
	}
	credential.CreatedAt = parseTime(created)
	return credential, nil
}

func (r *ISCSISecurityRepo) ListCredentials(ctx context.Context) ([]domain.ISCSICredential, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT credential_id, label, username, mutual_username, encrypted_secret, version, created_at FROM iscsi_chap_credentials ORDER BY credential_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.ISCSICredential, 0)
	for rows.Next() {
		var credential domain.ISCSICredential
		var created string
		if err := rows.Scan(&credential.CredentialID, &credential.Label, &credential.Username, &credential.MutualUsername, &credential.EncryptedSecret, &credential.Version, &created); err != nil {
			return nil, err
		}
		credential.CreatedAt = parseTime(created)
		out = append(out, credential)
	}
	return out, rows.Err()
}

func (r *ISCSISecurityRepo) DeleteCredential(ctx context.Context, credentialID string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM iscsi_chap_credentials WHERE credential_id=?`, credentialID)
	return checkSecurityDelete(result, err)
}

func (r *ISCSISecurityRepo) AppendSnapshot(ctx context.Context, snapshot domain.ISCSISecuritySnapshot) error {
	if snapshot.Validate() != nil {
		return domain.ErrInvalidInput
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var maxVersion int64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM iscsi_security_snapshots WHERE scope=? AND owner_id=?`, snapshot.Scope, snapshot.OwnerID).Scan(&maxVersion)
	if err != nil {
		return err
	}
	if snapshot.Version != maxVersion+1 {
		return domain.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO iscsi_security_snapshots(snapshot_id, scope, owner_id, version, payload, created_by, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)`, snapshot.SnapshotID, snapshot.Scope, snapshot.OwnerID, snapshot.Version, snapshot.Payload, snapshot.CreatedBy, formatTime(snapshot.CreatedAt)); err != nil {
		return mapSecurityRepoError(err)
	}
	for _, id := range uniqueValues(snapshot.CredentialRefs) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO iscsi_snapshot_credentials(snapshot_id, credential_id) VALUES(?, ?)`, snapshot.SnapshotID, id); err != nil {
			return mapSecurityRepoError(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM iscsi_security_snapshots WHERE scope=? AND owner_id=? AND version <= ?`, snapshot.Scope, snapshot.OwnerID, snapshot.Version-maxISCSISecurityHistory); err != nil {
		return err
	}
	return mapSecurityRepoError(tx.Commit())
}

func (r *ISCSISecurityRepo) ListSnapshots(ctx context.Context, scope, ownerID string) ([]domain.ISCSISecuritySnapshot, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT snapshot_id, scope, owner_id, version, payload, created_by, created_at FROM iscsi_security_snapshots WHERE scope=? AND owner_id=? ORDER BY version`, scope, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.ISCSISecuritySnapshot, 0)
	for rows.Next() {
		var snapshot domain.ISCSISecuritySnapshot
		var createdAt string
		if err := rows.Scan(&snapshot.SnapshotID, &snapshot.Scope, &snapshot.OwnerID, &snapshot.Version, &snapshot.Payload, &snapshot.CreatedBy, &createdAt); err != nil {
			return nil, err
		}
		snapshot.CreatedAt = parseTime(createdAt)
		out = append(out, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].CredentialRefs, err = listSnapshotReferences(ctx, r.db, `SELECT credential_id FROM iscsi_snapshot_credentials WHERE snapshot_id=?`, out[i].SnapshotID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func listSnapshotReferences(ctx context.Context, db *sql.DB, query, snapshotID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

func readBindingGeneration(ctx context.Context, tx *sql.Tx, table, keyColumn, key string) (bool, int64, error) {
	var generation int64
	err := tx.QueryRowContext(ctx, `SELECT generation FROM `+table+` WHERE `+keyColumn+`=?`, key).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	return err == nil, generation, err
}

func securityBindingTable(binding domain.ISCSISecurityBinding) (table, keyColumn, key string) {
	switch binding.Scope {
	case domain.SecurityScopeLibrary:
		return "library_iscsi_security", "library_id", binding.OwnerID
	case domain.SecurityScopeDrive:
		return "drive_iscsi_security", "drive_id", binding.OwnerID
	case domain.SecurityScopeTarget:
		return "target_iscsi_security", "target_iqn", binding.TargetIQN
	default:
		return "", "", ""
	}
}

func uniqueValues(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok || value == "" {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func checkSecurityDelete(result sql.Result, err error) error {
	if err != nil {
		return mapSecurityRepoError(err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func mapSecurityRepoError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	if isSQLiteConstraint(err) {
		return fmt.Errorf("%w: security reference or uniqueness constraint", domain.ErrConflict)
	}
	return err
}
