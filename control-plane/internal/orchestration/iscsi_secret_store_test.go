package orchestration

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestISCSISecretStoreUsesFreshAESGCMNoncesAndAuthenticatedIdentity(t *testing.T) {
	store := newTestISCSISecretStore(t, filepath.Join(t.TempDir(), "iscsi-secrets.key"))
	secret := domain.ISCSISecret{Username: "backup-user", Secret: "private-secret-123"}
	first, err := store.SealCredential(secret, "cred-a", 1, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SealCredential(secret, "cred-a", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("same plaintext should receive a fresh nonce")
	}
	credential := domain.ISCSICredential{CredentialID: "cred-a", Label: "backup", Username: secret.Username, EncryptedSecret: first, Version: 1}
	opened, err := store.OpenCredential(credential)
	if err != nil || opened != secret {
		t.Fatalf("credential round trip mismatch: %#v, %v", opened, err)
	}
	if _, err := store.Open("chap-credential", "cred-b", 1, first); !errors.Is(err, ErrISCSISecretProtected) {
		t.Fatalf("object identity must be authenticated, got %v", err)
	}
	if _, err := store.Open("chap-credential", "cred-a", 2, first); !errors.Is(err, ErrISCSISecretProtected) {
		t.Fatalf("version must be authenticated, got %v", err)
	}
	tampered := append([]byte(nil), first...)
	tampered[len(tampered)-1] ^= 0x80
	if _, err := store.Open("chap-credential", "cred-a", 1, tampered); !errors.Is(err, ErrISCSISecretProtected) {
		t.Fatalf("tampering should fail closed, got %v", err)
	}
}

func TestISCSISecretStoreDoesNotRegenerateMissingProtectedCatalogKey(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "iscsi-secrets.key")
	store := newTestISCSISecretStore(t, keyPath)
	if _, err := store.Seal("chap-credential", "cred-a", 1, []byte("protected"), true); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Seal("chap-credential", "cred-b", 1, []byte("new"), false); !errors.Is(err, ErrISCSISecretKeyMissing) {
		t.Fatalf("existing catalog must not silently provision replacement key, got %v", err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement key was unexpectedly created: %v", err)
	}
}

func TestISCSISecretStoreRejectsSymlinkAndInvalidKeyLength(t *testing.T) {
	dir := t.TempDir()
	actual := filepath.Join(dir, "actual")
	if err := os.WriteFile(actual, make([]byte, 32), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "iscsi-secrets.key")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	store := newTestISCSISecretStore(t, link)
	if _, err := store.loadKey(false); !errors.Is(err, ErrISCSISecretProtected) {
		t.Fatalf("key symlink must be rejected, got %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, make([]byte, 31), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(link, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := store.loadKey(false); !errors.Is(err, ErrISCSISecretProtected) {
		t.Fatalf("invalid master key length must be rejected, got %v", err)
	}
}

func newTestISCSISecretStore(t *testing.T, keyPath string) *ISCSISecretStore {
	t.Helper()
	store, err := NewISCSISecretStore(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	store.validateDir = func(string) error { return nil }
	store.validateOwner = func(_ string, info os.FileInfo) error {
		if info.Mode().Perm() != 0o640 || !info.Mode().IsRegular() {
			return ErrISCSISecretProtected
		}
		return nil
	}
	store.setOwner = func(string) error { return nil }
	return store
}
