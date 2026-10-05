package storageutil

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var ErrStorageLockBusy = errors.New("storage layout is busy")
var ErrStorageIdentityMismatch = errors.New("storage root identity changed")

type StorageLease struct {
	file       *os.File
	rootPath   string
	rootDevice uint64
	rootInode  uint64
}

func LayoutLeaseKey(device, inode uint64, cartridgeID string) string {
	payload := strings.Join([]string{
		"holo-layout-v2",
		strconv.FormatUint(device, 10),
		strconv.FormatUint(inode, 10),
		SanitizeLayoutID(cartridgeID),
	}, "\x00")
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func AcquireLayoutLease(poolRoot, cartridgeID string) (*StorageLease, error) {
	if err := validateLeaseID(cartridgeID); err != nil {
		return nil, err
	}
	root, device, inode, err := openStorageRootIdentity(poolRoot)
	if err != nil {
		return nil, err
	}
	key := LayoutLeaseKey(device, inode, cartridgeID)
	lease, err := acquireStorageLock(filepath.Join(storageLockDir(), "layout-"+key+".lock"))
	if err != nil {
		return nil, err
	}
	lease.rootPath = root
	lease.rootDevice = device
	lease.rootInode = inode
	return lease, nil
}

func AcquireFilesystemLock(poolRoot string) (*StorageLease, error) {
	_, device, _, err := openStorageRootIdentity(poolRoot)
	if err != nil {
		return nil, err
	}
	return acquireStorageLock(filepath.Join(storageLockDir(), fmt.Sprintf("fs-%d.lock", device)))
}

func (lease *StorageLease) VerifyPoolRoot(poolRoot string) error {
	if lease == nil || lease.file == nil || lease.rootPath == "" {
		return ErrStorageIdentityMismatch
	}
	_, device, inode, err := openStorageRootIdentity(poolRoot)
	if err != nil {
		return ErrStorageIdentityMismatch
	}
	if device != lease.rootDevice || inode != lease.rootInode {
		return ErrStorageIdentityMismatch
	}
	return nil
}

func (lease *StorageLease) Release() error {
	if lease == nil || lease.file == nil {
		return nil
	}
	file := lease.file
	lease.file = nil
	err := unix.Flock(int(file.Fd()), unix.LOCK_UN)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func acquireStorageLock(path string) (*StorageLease, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("storage lock directory is unsafe")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err = file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("storage lock path is not a regular file")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrStorageLockBusy
		}
		return nil, err
	}
	return &StorageLease{file: file}, nil
}

func openStorageRootIdentity(poolRoot string) (string, uint64, uint64, error) {
	root, err := filepath.EvalSymlinks(strings.TrimSpace(poolRoot))
	if err != nil {
		return "", 0, 0, fmt.Errorf("resolve pool root: %w", err)
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", 0, 0, fmt.Errorf("open pool root: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return "", 0, 0, fmt.Errorf("inspect pool root: %w", err)
	}
	_ = unix.Close(fd)
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", 0, 0, fmt.Errorf("pool root is not a directory")
	}
	return root, uint64(stat.Dev), uint64(stat.Ino), nil
}

func storageLockDir() string {
	if root := strings.TrimSpace(os.Getenv("HOLO_RUN_DIR")); root != "" {
		return filepath.Join(root, "storage-locks")
	}
	return "/run/holo/storage-locks"
}

func validateLeaseID(value string) error {
	if value == "" || len(value) > 128 || strings.Contains(value, "..") || !isLeaseIDByte(value[0], true) {
		return fmt.Errorf("invalid storage lease identity")
	}
	for index := 1; index < len(value); index++ {
		if !isLeaseIDByte(value[index], false) {
			return fmt.Errorf("invalid storage lease identity")
		}
	}
	return nil
}

func isLeaseIDByte(value byte, first bool) bool {
	if value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= '0' && value <= '9' {
		return true
	}
	return !first && strings.ContainsRune("._: -", rune(value))
}
