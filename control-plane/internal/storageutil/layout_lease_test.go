package storageutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLayoutLeaseKeyMatchesRustGoldenVector(t *testing.T) {
	if got, want := LayoutLeaseKey(2049, 123456, "VTL000001"), "80740740baf2b7e51e0efa9d98382f76ecaa360969bdcff495e76784eec2b78c"; got != want {
		t.Fatalf("LayoutLeaseKey() = %q, want %q", got, want)
	}
}

func TestLayoutLeaseIdentityValidationMatchesRustRules(t *testing.T) {
	for _, value := range []string{" library-a", "", "../library", "library/a", "library\n"} {
		if err := validateLeaseID(value); err == nil {
			t.Errorf("validateLeaseID(%q) accepted an identity rejected by Rust", value)
		}
	}
	for _, value := range []string{"library-a", "library a", "library:a"} {
		if err := validateLeaseID(value); err != nil {
			t.Errorf("validateLeaseID(%q) rejected an identity accepted by Rust: %v", value, err)
		}
	}
}

func TestLayoutLeaseSerializesTheSameCartridgeAndUsesDistinctIdentities(t *testing.T) {
	root := t.TempDir()
	runtime := t.TempDir()
	t.Setenv("HOLO_RUN_DIR", runtime)
	first, err := AcquireLayoutLease(root, "cart-a")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if _, err := AcquireLayoutLease(root, "cart-a"); !errors.Is(err, ErrStorageLockBusy) {
		t.Fatalf("second same-layout lock error = %v, want busy", err)
	}
	second, err := AcquireLayoutLease(root, "cart-b")
	if err != nil {
		t.Fatalf("different cartridge lock: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLayoutLeaseRejectsSymlinkLockDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "pool")
	runtime := filepath.Join(base, "run")
	other := filepath.Join(base, "other")
	for _, path := range []string{root, runtime, other} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(other, filepath.Join(runtime, "storage-locks")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOLO_RUN_DIR", runtime)
	if _, err := AcquireLayoutLease(root, "cart-a"); err == nil {
		t.Fatal("expected unsafe lock directory to be rejected")
	}
}

func TestLayoutLeaseAliasesAndRootReplacement(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "pool")
	alias := filepath.Join(base, "pool-alias")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOLO_RUN_DIR", t.TempDir())
	lease, err := AcquireLayoutLease(root, "VTA000L06")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if _, err := AcquireLayoutLease(alias, "VTA000L06"); !errors.Is(err, ErrStorageLockBusy) {
		t.Fatalf("symlink alias should share cartridge lease, got %v", err)
	}
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := lease.VerifyPoolRoot(root); !errors.Is(err, ErrStorageIdentityMismatch) {
		t.Fatalf("replaced storage root should fail identity verification, got %v", err)
	}
}

func TestLayoutLeaseSerializesAliasAcrossProcesses(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "pool")
	alias := filepath.Join(base, "pool-alias")
	runtime := filepath.Join(base, "run")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOLO_RUN_DIR", runtime)
	lease, err := AcquireLayoutLease(root, "cart-a")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	command := exec.Command(os.Args[0], "-test.run=^TestLayoutLeaseSubprocessProbe$")
	command.Env = append(os.Environ(),
		"HOLO_LAYOUT_LEASE_PROBE_ROOT="+alias,
		"HOLO_LAYOUT_LEASE_PROBE_CARTRIDGE=cart-a",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("child process did not observe the same locked inode: %v output=%s", err, output)
	}
}

func TestLayoutLeaseSubprocessProbe(t *testing.T) {
	root := os.Getenv("HOLO_LAYOUT_LEASE_PROBE_ROOT")
	if root == "" {
		return
	}
	if _, err := AcquireLayoutLease(root, os.Getenv("HOLO_LAYOUT_LEASE_PROBE_CARTRIDGE")); !errors.Is(err, ErrStorageLockBusy) {
		t.Fatalf("child process expected busy shared lease, got %v", err)
	}
}

func TestLayoutLeaseSerializesAliasAgainstRust(t *testing.T) {
	rustTestBinary := os.Getenv("HOLO_RUST_LEASE_TEST_BIN")
	if rustTestBinary == "" {
		t.Skip("set HOLO_RUST_LEASE_TEST_BIN to run the cross-language Linux probe")
	}
	base := t.TempDir()
	root := filepath.Join(base, "pool")
	alias := filepath.Join(base, "pool-alias")
	runtime := filepath.Join(base, "run")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireLayoutLease(root, "VTA000L06")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	command := exec.Command(rustTestBinary,
		"--exact",
		"storage::layout_lease_tests::layout_lease_child_process_observes_shared_lock",
		"--nocapture",
	)
	command.Env = append(os.Environ(),
		"HOLO_LAYOUT_LEASE_CHILD_ROOT="+alias,
		"HOLO_LAYOUT_LEASE_CHILD_RUNTIME="+runtime,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Rust process did not observe the Go lease: %v output=%s", err, output)
	}
}

func TestFilesystemLockAliasesShareOneDeviceLock(t *testing.T) {
	base := t.TempDir()
	poolA := filepath.Join(base, "pool-a")
	poolB := filepath.Join(base, "pool-b")
	if err := os.Mkdir(poolA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(poolB, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOLO_RUN_DIR", t.TempDir())
	first, err := AcquireFilesystemLock(poolA)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if _, err := AcquireFilesystemLock(poolB); !errors.Is(err, ErrStorageLockBusy) {
		t.Fatalf("filesystem alias lock error = %v, want busy", err)
	}
}
