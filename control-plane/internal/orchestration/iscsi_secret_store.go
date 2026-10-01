package orchestration

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

const (
	iscsiMasterKeyBytes = 32
	iscsiVaultHeader    = "HISC\x01"
	maxSecretPayload    = 1 << 20
)

var (
	ErrISCSISecretKeyMissing = errors.New("iSCSI secret key unavailable")
	ErrISCSISecretProtected  = errors.New("iSCSI secret could not be protected")
)

type ISCSISecretStore struct {
	keyPath       string
	validateOwner func(string, os.FileInfo) error
	validateDir   func(string) error
	setOwner      func(string) error
}

type credentialSecretPayload struct {
	Username       string `json:"username"`
	Secret         string `json:"secret"`
	MutualUsername string `json:"mutualUsername,omitempty"`
	MutualSecret   string `json:"mutualSecret,omitempty"`
}

func NewISCSISecretStore(keyPath string) (*ISCSISecretStore, error) {
	if !filepath.IsAbs(keyPath) {
		return nil, domain.ErrInvalidInput
	}
	return &ISCSISecretStore{keyPath: filepath.Clean(keyPath), validateOwner: validateRootHoloKey, validateDir: validateRootControlledDirectory, setOwner: setRootHoloOwner}, nil
}

// Seal creates a versioned AES-256-GCM envelope. Provisioning is permitted
// only when the caller has established that the protected catalog is empty.
func (s *ISCSISecretStore) Seal(objectType, objectID string, version int64, plaintext []byte, allowProvision bool) ([]byte, error) {
	if s == nil || !validISCSISecretObject(objectType, objectID, version) || len(plaintext) == 0 || len(plaintext) > maxSecretPayload {
		return nil, ErrISCSISecretProtected
	}
	key, err := s.loadKey(allowProvision)
	if err != nil {
		return nil, err
	}
	defer zeroBytes(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrISCSISecretProtected
	}
	aead, err := cipher.NewGCMWithNonceSize(block, 12)
	if err != nil {
		return nil, ErrISCSISecretProtected
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, ErrISCSISecretProtected
	}
	result := make([]byte, len(iscsiVaultHeader), len(iscsiVaultHeader)+len(nonce)+len(plaintext)+aead.Overhead())
	copy(result, iscsiVaultHeader)
	result = append(result, nonce...)
	result = aead.Seal(result, nonce, plaintext, secretAAD(objectType, objectID, version))
	return result, nil
}

func (s *ISCSISecretStore) SealCredential(secret domain.ISCSISecret, credentialID string, version int64, allowProvision bool) ([]byte, error) {
	if secret.Validate() != nil {
		return nil, domain.ErrInvalidInput
	}
	plaintext, err := json.Marshal(credentialSecretPayload{Username: secret.Username, Secret: secret.Secret, MutualUsername: secret.MutualUsername, MutualSecret: secret.MutualSecret})
	if err != nil {
		return nil, ErrISCSISecretProtected
	}
	defer zeroBytes(plaintext)
	return s.Seal("chap-credential", credentialID, version, plaintext, allowProvision)
}

func (s *ISCSISecretStore) Open(objectType, objectID string, version int64, envelope []byte) ([]byte, error) {
	if s == nil || !validISCSISecretObject(objectType, objectID, version) || len(envelope) < len(iscsiVaultHeader)+12+16+1 || len(envelope) > maxSecretPayload+64 {
		return nil, ErrISCSISecretProtected
	}
	if subtle.ConstantTimeCompare(envelope[:len(iscsiVaultHeader)], []byte(iscsiVaultHeader)) != 1 {
		return nil, ErrISCSISecretProtected
	}
	key, err := s.loadKey(false)
	if err != nil {
		return nil, err
	}
	defer zeroBytes(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrISCSISecretProtected
	}
	aead, err := cipher.NewGCMWithNonceSize(block, 12)
	if err != nil {
		return nil, ErrISCSISecretProtected
	}
	start := len(iscsiVaultHeader)
	nonce := envelope[start : start+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, envelope[start+aead.NonceSize():], secretAAD(objectType, objectID, version))
	if err != nil || len(plaintext) == 0 || len(plaintext) > maxSecretPayload {
		zeroBytes(plaintext)
		return nil, ErrISCSISecretProtected
	}
	return plaintext, nil
}

func (s *ISCSISecretStore) OpenCredential(credential domain.ISCSICredential) (domain.ISCSISecret, error) {
	if credential.Validate() != nil {
		return domain.ISCSISecret{}, ErrISCSISecretProtected
	}
	plaintext, err := s.Open("chap-credential", credential.CredentialID, credential.Version, credential.EncryptedSecret)
	if err != nil {
		return domain.ISCSISecret{}, err
	}
	defer zeroBytes(plaintext)
	var payload credentialSecretPayload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return domain.ISCSISecret{}, ErrISCSISecretProtected
	}
	secret := domain.ISCSISecret{Username: payload.Username, Secret: payload.Secret, MutualUsername: payload.MutualUsername, MutualSecret: payload.MutualSecret}
	if secret.Validate() != nil || secret.Username != credential.Username || secret.MutualUsername != credential.MutualUsername {
		return domain.ISCSISecret{}, ErrISCSISecretProtected
	}
	return secret, nil
}

func (s *ISCSISecretStore) loadKey(allowProvision bool) ([]byte, error) {
	info, err := os.Lstat(s.keyPath)
	if errors.Is(err, os.ErrNotExist) {
		if !allowProvision {
			return nil, ErrISCSISecretKeyMissing
		}
		if err := s.createKey(); err != nil {
			return nil, ErrISCSISecretProtected
		}
		info, err = os.Lstat(s.keyPath)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || s.validateOwner == nil || s.validateOwner(s.keyPath, info) != nil {
		return nil, ErrISCSISecretProtected
	}
	f, err := os.Open(s.keyPath)
	if err != nil {
		return nil, ErrISCSISecretProtected
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return nil, ErrISCSISecretProtected
	}
	key, err := io.ReadAll(io.LimitReader(f, iscsiMasterKeyBytes+1))
	if err != nil || len(key) != iscsiMasterKeyBytes {
		zeroBytes(key)
		return nil, ErrISCSISecretProtected
	}
	return key, nil
}

func (s *ISCSISecretStore) createKey() error {
	dir := filepath.Dir(s.keyPath)
	if s.validateDir == nil || s.validateDir(dir) != nil {
		return ErrISCSISecretProtected
	}
	key := make([]byte, iscsiMasterKeyBytes)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return err
	}
	defer zeroBytes(key)
	f, err := os.OpenFile(s.keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(s.keyPath)
		}
	}()
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if s.setOwner == nil || s.setOwner(s.keyPath) != nil {
		return ErrISCSISecretProtected
	}
	if err := os.Chmod(s.keyPath, 0o640); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func secretAAD(objectType, objectID string, version int64) []byte {
	return []byte("holo-iscsi\x00" + objectType + "\x00" + objectID + "\x00" + strconv.FormatInt(version, 10))
}

func validISCSISecretObject(objectType, objectID string, version int64) bool {
	return objectType == "chap-credential" && domain.ValidateManagementID(objectID) == nil && version > 0
}

func validateRootControlledDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return ErrISCSISecretProtected
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return ErrISCSISecretProtected
	}
	return nil
}

func validateRootHoloKey(path string, info os.FileInfo) error {
	if info.Mode().Perm() != 0o640 || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ErrISCSISecretProtected
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || stat.Uid != 0 {
		return ErrISCSISecretProtected
	}
	group, err := user.LookupGroup("holo")
	if err != nil {
		return ErrISCSISecretProtected
	}
	groupID, err := strconv.ParseUint(group.Gid, 10, 32)
	if err != nil || uint64(stat.Gid) != groupID {
		return ErrISCSISecretProtected
	}
	return validateRootControlledDirectory(filepath.Dir(path))
}

func setRootHoloOwner(path string) error {
	group, err := user.LookupGroup("holo")
	if err != nil {
		return err
	}
	groupID, err := strconv.Atoi(group.Gid)
	if err != nil {
		return err
	}
	return os.Chown(path, 0, groupID)
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
