package orchestration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

type fakeStorageRunner struct {
	output      string
	fstype      string
	mountSource string
	mountTarget string
	dfOutput    string
	err         error
	failCommand string
	commands    []string
}

func (r *fakeStorageRunner) Run(_ context.Context, command string, args ...string) (string, error) {
	r.commands = append(r.commands, strings.TrimSpace(command+" "+strings.Join(args, " ")))
	if r.err != nil {
		return "", r.err
	}
	if command == "sudo" && len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	if strings.HasSuffix(command, "/holo-storage-helper") && len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	if r.failCommand != "" && command == r.failCommand {
		return "", errors.New("forced command failure")
	}
	if command == "lsblk" {
		if len(args) >= 2 && args[0] == "-no" && args[1] == "FSTYPE" {
			return r.fstype, nil
		}
		return r.output, nil
	}
	if command == "findmnt" {
		if len(args) >= 3 && args[0] == "-rn" && args[1] == "-M" {
			return r.mountSource, nil
		}
		if len(args) >= 3 && args[0] == "-rn" && args[1] == "-S" {
			return r.mountTarget, nil
		}
	}
	if command == "df" {
		return r.dfOutput, nil
	}
	if command == "mount" && len(args) >= 2 {
		r.mountSource = args[len(args)-2]
		return "", nil
	}
	if command == "umount" {
		r.mountSource = ""
		return "", nil
	}
	return "", nil
}

func TestOSStorageCommandRunnerIgnoresSuccessfulStderr(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "warn-success")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'sudo: unable to send audit message: Operation not permitted' >&2\nprintf '\\n'\n"), 0o700); err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	out, err := (&osStorageCommandRunner{}).Run(context.Background(), script)
	if err != nil {
		t.Fatalf("run command: %v", err)
	}
	if out != "\n" {
		t.Fatalf("expected stdout only, got %q", out)
	}
}

const sampleLsblk = `{
  "blockdevices": [
    {
      "name": "sda",
      "path": "/dev/sda",
      "type": "disk",
      "size": "536870912000",
      "mountpoint": null,
      "fstype": "",
      "model": "SYSTEM",
      "serial": "SYS001",
      "vendor": "ATA",
      "children": [
        {"name": "sda1", "path": "/dev/sda1", "type": "part", "size": "1073741824", "mountpoint": "/boot", "fstype": "ext4"}
      ]
    },
    {
      "name": "sdb",
      "path": "/dev/sdb",
      "type": "disk",
      "size": "1099511627776",
      "mountpoint": null,
      "fstype": "",
      "model": "DATA",
      "serial": "DATA001",
      "vendor": "ATA"
    }
  ]
}`

const sampleLsblkWholeDiskXfs = `{
  "blockdevices": [
    {
      "name": "sdb",
      "path": "/dev/sdb",
      "type": "disk",
      "size": "1099511627776",
      "mountpoint": null,
      "fstype": "xfs",
      "model": "DATA",
      "serial": "DATA001",
      "vendor": "ATA"
    }
  ]
}`

const sampleLsblkWithoutPath = `{
  "blockdevices": [
    {
      "name": "sdb",
      "type": "disk",
      "size": "1099511627776",
      "mountpoint": null,
      "fstype": "",
      "model": "DATA",
      "serial": "DATA001",
      "vendor": "ATA"
    }
  ]
}`

const sampleLsblkWholeDiskExt4 = `{
  "blockdevices": [
    {
      "name": "sdb",
      "path": "/dev/sdb",
      "type": "disk",
      "size": "1099511627776",
      "mountpoint": null,
      "fstype": "ext4",
      "model": "DATA",
      "serial": "DATA001",
      "vendor": "ATA"
    }
	  ]
	}`

const sampleLsblkTwoDataDisks = `{
  "blockdevices": [
    {
      "name": "sdb",
      "path": "/dev/sdb",
      "type": "disk",
      "size": "1099511627776",
      "mountpoint": null,
      "fstype": "",
      "model": "DATA",
      "serial": "DATA001",
      "vendor": "ATA"
    },
    {
      "name": "sdc",
      "path": "/dev/sdc",
      "type": "disk",
      "size": "1099511627776",
      "mountpoint": null,
      "fstype": "",
      "model": "DATA",
      "serial": "DATA002",
      "vendor": "ATA"
    }
  ]
}`

func newStorageSvcForTest() *StorageManagementService {
	repo := memory.NewStoragePoolRepo()
	return NewStorageManagementService(repo, audit.NewMemoryWriter(), &fakeStorageRunner{output: sampleLsblk})
}

func hasRecordedCommand(commands []string, want string) bool {
	for _, command := range commands {
		if command == want {
			return true
		}
	}
	return false
}

func TestStorageManagementService_StorageAuditEventIDsAreUnique(t *testing.T) {
	writer := audit.NewMemoryWriter()
	svc := NewStorageManagementService(memory.NewStoragePoolRepo(), writer, &fakeStorageRunner{output: sampleLsblk})
	svc.nowFn = func() time.Time { return time.Unix(123, 0).UTC() }

	svc.emitStorageAudit(context.Background(), "system", "storage_pool_create", "pool-1", "success", nil)
	svc.emitStorageAudit(context.Background(), "system", "storage_pool_create", "pool-1", "success", nil)

	events := writer.Events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].EventID == events[1].EventID {
		t.Fatalf("expected unique event IDs, got duplicate %q", events[0].EventID)
	}
}

func TestStorageManagementService_DiscoverDisksClassifiesAvailability(t *testing.T) {
	svc := newStorageSvcForTest()
	ctx := context.Background()

	disks, err := svc.DiscoverDisks(ctx)
	if err != nil {
		t.Fatalf("discover disks failed: %v", err)
	}
	if len(disks) != 1 {
		t.Fatalf("expected only attachable/managed disks to be listed, got %d", len(disks))
	}

	if disks[0].DevicePath != "/dev/sdb" {
		t.Fatalf("expected only /dev/sdb to be listed, got %+v", disks)
	}
	if disks[0].Availability != domain.DiskAvailable {
		t.Fatalf("expected /dev/sdb available, got %+v", disks[0])
	}
}

func TestStorageManagementService_DiscoverDisksKeepsUnmountedWholeDiskXfsAvailable(t *testing.T) {
	repo := memory.NewStoragePoolRepo()
	svc := NewStorageManagementService(repo, audit.NewMemoryWriter(), &fakeStorageRunner{output: sampleLsblkWholeDiskXfs})
	ctx := context.Background()

	disks, err := svc.DiscoverDisks(ctx)
	if err != nil {
		t.Fatalf("discover disks failed: %v", err)
	}
	if len(disks) != 1 {
		t.Fatalf("expected reusable whole-disk xfs to be listed, got %d", len(disks))
	}
	if disks[0].Availability != domain.DiskAvailable {
		t.Fatalf("expected whole-disk xfs disk available, got %+v", disks[0])
	}
}

func TestStorageManagementService_DiscoverDisksSynthesizesPathForOlderLsblk(t *testing.T) {
	repo := memory.NewStoragePoolRepo()
	runner := &fakeStorageRunner{output: sampleLsblkWithoutPath}
	svc := NewStorageManagementService(repo, audit.NewMemoryWriter(), runner)
	ctx := context.Background()

	disks, err := svc.DiscoverDisks(ctx)
	if err != nil {
		t.Fatalf("discover disks failed: %v", err)
	}
	if len(disks) != 1 {
		t.Fatalf("expected one disk, got %d", len(disks))
	}
	if got := disks[0].DevicePath; got != "/dev/sdb" {
		t.Fatalf("expected synthesized /dev/sdb path, got %q", got)
	}
	if !hasRecordedCommand(runner.commands, "lsblk -J -b -o NAME,TYPE,SIZE,MOUNTPOINT,FSTYPE,MODEL,SERIAL,VENDOR") {
		t.Fatalf("expected lsblk command without PATH column, got %#v", runner.commands)
	}
}

func TestStorageManagementService_DiscoverDisksMarksUnsupportedFilesystemUnavailable(t *testing.T) {
	repo := memory.NewStoragePoolRepo()
	svc := NewStorageManagementService(repo, audit.NewMemoryWriter(), &fakeStorageRunner{output: sampleLsblkWholeDiskExt4})
	ctx := context.Background()

	disks, err := svc.DiscoverDisks(ctx)
	if err != nil {
		t.Fatalf("discover disks failed: %v", err)
	}
	if len(disks) != 1 {
		t.Fatalf("expected unsupported whole-disk filesystem to remain visible, got %d", len(disks))
	}
	if disks[0].Availability != domain.DiskUnavailable {
		t.Fatalf("expected unsupported whole-disk filesystem unavailable, got %+v", disks[0])
	}
	if got := disks[0].UnavailableReason; got != "unsupported filesystem signature detected: ext4" {
		t.Fatalf("unexpected unavailable reason %q", got)
	}
}

func TestStorageManagementService_PoolDiskLifecycleAndSafety(t *testing.T) {
	svc := newStorageSvcForTest()
	ctx := context.Background()

	pool, err := svc.CreatePool(ctx, CreateStoragePoolRequest{PoolID: "pool-1", Name: "Pool 1", WarningThresholdPct: 90, Actor: "tester"})
	if err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	if pool.PoolID != "pool-1" {
		t.Fatalf("unexpected pool id %s", pool.PoolID)
	}

	attached, err := svc.AttachDisk(ctx, "pool-1", "/dev/sdb", "tester")
	if err != nil {
		t.Fatalf("attach disk failed: %v", err)
	}
	if len(attached.Disks) != 1 {
		t.Fatalf("expected one attached disk, got %d", len(attached.Disks))
	}

	if _, err := svc.AttachDisk(ctx, "pool-1", "/dev/sdb", "tester"); !errors.Is(err, domain.ErrInvalidState) && !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected attach conflict/invalid state, got %v", err)
	}

	if _, _, err := svc.ReserveWrite(ctx, "pool-1", attached.Capacity.TotalBytes); err != nil {
		t.Fatalf("reserve write to full capacity failed: %v", err)
	}
	if _, err := svc.DetachDisk(ctx, "pool-1", "/dev/sdb", "tester"); err != domain.ErrInvalidState {
		t.Fatalf("expected invalid state on detaching last disk with used data, got %v", err)
	}

	if err := svc.RollbackReservedWrite(ctx, "pool-1", attached.Capacity.TotalBytes); err != nil {
		t.Fatalf("rollback reserve failed: %v", err)
	}
	if _, err := svc.DetachDisk(ctx, "pool-1", "/dev/sdb", "tester"); err != nil {
		t.Fatalf("detach after rollback failed: %v", err)
	}
}

func TestStorageManagementService_RejectsUnsafeDevicePath(t *testing.T) {
	svc := newStorageSvcForTest()
	ctx := context.Background()
	if _, err := svc.CreatePool(ctx, CreateStoragePoolRequest{PoolID: "pool-unsafe", Name: "Pool", Actor: "tester"}); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}

	for _, path := range []string{"../sdb", "/dev/disk/by-id/example", "/tmp/sdb", "sdb/../sdc"} {
		if _, err := svc.AttachDisk(ctx, "pool-unsafe", path, "tester"); !errors.Is(err, domain.ErrInvalidInput) {
			t.Fatalf("expected invalid input for %q, got %v", path, err)
		}
	}
}

func TestStorageManagementService_ListPoolsUsesMountedFilesystemCapacity(t *testing.T) {
	repo := memory.NewStoragePoolRepo()
	runner := &fakeStorageRunner{
		mountSource: "/dev/sdb",
		dfOutput:    "1B-blocks Used Avail\n209715200 73400320 136314880\n",
	}
	svc := NewStorageManagementService(repo, audit.NewMemoryWriter(), runner)
	ctx := context.Background()

	pool, err := domain.NewStoragePoolRuntime("pool-df", "Pool DF", 90)
	if err != nil {
		t.Fatalf("create pool runtime failed: %v", err)
	}
	if err := repo.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	if _, err := repo.AttachDisk(ctx, "pool-df", domain.StoragePoolDisk{DevicePath: "/dev/sdb", SizeBytes: 1099511627776}); err != nil {
		t.Fatalf("attach disk failed: %v", err)
	}

	pools := svc.ListPools(ctx)
	if len(pools) != 1 {
		t.Fatalf("expected one pool, got %d", len(pools))
	}
	if got := pools[0].Capacity.TotalBytes; got != 209715200 {
		t.Fatalf("expected df total bytes, got %d", got)
	}
	if got := pools[0].Capacity.UsedBytes; got != 73400320 {
		t.Fatalf("expected df used bytes, got %d", got)
	}
	if got := pools[0].Capacity.FreeBytes; got != 136314880 {
		t.Fatalf("expected df free bytes, got %d", got)
	}

	snapshot, err := svc.GetCapacity(ctx, "pool-df")
	if err != nil {
		t.Fatalf("get capacity failed: %v", err)
	}
	if snapshot.UsedPercent != 35 {
		t.Fatalf("expected df used percent 35, got %d", snapshot.UsedPercent)
	}
}

func TestStorageManagementService_AttachFormatsNewPoolWithXFS(t *testing.T) {
	repo := memory.NewStoragePoolRepo()
	runner := &fakeStorageRunner{output: sampleLsblk}
	svc := NewStorageManagementService(repo, audit.NewMemoryWriter(), runner)
	ctx := context.Background()

	if _, err := svc.CreatePool(ctx, CreateStoragePoolRequest{PoolID: "pool-xfs", Name: "Pool XFS", WarningThresholdPct: 90, Actor: "tester"}); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	if _, err := svc.AttachDisk(ctx, "pool-xfs", "/dev/sdb", "tester"); err != nil {
		t.Fatalf("attach disk failed: %v", err)
	}
	if !hasRecordedCommand(runner.commands, "sudo mkfs.xfs -f /dev/sdb") {
		t.Fatalf("expected mkfs.xfs command, got %#v", runner.commands)
	}
}

func TestStorageManagementService_PrivilegedHelperWrapsStorageCommands(t *testing.T) {
	t.Setenv("HOLO_STORAGE_PRIVILEGED_HELPER", "/opt/holo/bin/holo-storage-helper")
	repo := memory.NewStoragePoolRepo()
	runner := &fakeStorageRunner{output: sampleLsblk}
	svc := NewStorageManagementService(repo, audit.NewMemoryWriter(), runner)
	ctx := context.Background()

	if _, err := svc.CreatePool(ctx, CreateStoragePoolRequest{PoolID: "pool-helper", Name: "Pool Helper", WarningThresholdPct: 90, Actor: "tester"}); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	if _, err := svc.AttachDisk(ctx, "pool-helper", "/dev/sdb", "tester"); err != nil {
		t.Fatalf("attach disk failed: %v", err)
	}
	if !hasRecordedCommand(runner.commands, "sudo /opt/holo/bin/holo-storage-helper mkfs.xfs -f /dev/sdb") {
		t.Fatalf("expected helper-wrapped mkfs.xfs command, got %#v", runner.commands)
	}
}

func TestStorageManagementService_EnsureAttachedPoolsMountedRemountsExistingXFS(t *testing.T) {
	repo := memory.NewStoragePoolRepo()
	runner := &fakeStorageRunner{
		output: sampleLsblkWholeDiskXfs,
		fstype: "xfs",
	}
	svc := NewStorageManagementService(repo, audit.NewMemoryWriter(), runner)
	ctx := context.Background()

	if _, err := svc.CreatePool(ctx, CreateStoragePoolRequest{PoolID: "pool-remount", Name: "Pool Remount", WarningThresholdPct: 90, Actor: "tester"}); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	if _, err := repo.AttachDisk(ctx, "pool-remount", domain.StoragePoolDisk{DevicePath: "/dev/sdb", SizeBytes: 1000}); err != nil {
		t.Fatalf("seed attached disk failed: %v", err)
	}
	if err := svc.EnsureAttachedPoolsMounted(ctx); err != nil {
		t.Fatalf("ensure attached pools mounted failed: %v", err)
	}
	if hasRecordedCommand(runner.commands, "sudo mkfs.xfs -f /dev/sdb") {
		t.Fatalf("did not expect existing XFS disk to be formatted, got %#v", runner.commands)
	}
	if !hasRecordedCommand(runner.commands, "sudo mount -o noatime,nodiratime /dev/sdb /var/lib/holo/storage-pools/pool-remount") {
		t.Fatalf("expected remount command, got %#v", runner.commands)
	}
}

func TestStorageManagementService_AttachUnmountsWhenMountSetupFails(t *testing.T) {
	repo := memory.NewStoragePoolRepo()
	runner := &fakeStorageRunner{output: sampleLsblk, failCommand: "chown"}
	svc := NewStorageManagementService(repo, audit.NewMemoryWriter(), runner)
	ctx := context.Background()

	if _, err := svc.CreatePool(ctx, CreateStoragePoolRequest{PoolID: "pool-rollback", Name: "Pool Rollback", WarningThresholdPct: 90, Actor: "tester"}); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	if _, err := svc.AttachDisk(ctx, "pool-rollback", "/dev/sdb", "tester"); err == nil {
		t.Fatalf("expected attach failure")
	}
	if !hasRecordedCommand(runner.commands, "sudo umount /var/lib/holo/storage-pools/pool-rollback") {
		t.Fatalf("expected mount rollback umount, got %#v", runner.commands)
	}
}

func TestStorageManagementService_StrictFlowRejectsSecondDiskWithoutAggregator(t *testing.T) {
	t.Setenv("HOLO_STRICT_STORAGE_FLOW", "1")
	repo := memory.NewStoragePoolRepo()
	runner := &fakeStorageRunner{output: sampleLsblkTwoDataDisks}
	svc := NewStorageManagementService(repo, audit.NewMemoryWriter(), runner)
	ctx := context.Background()

	if _, err := svc.CreatePool(ctx, CreateStoragePoolRequest{PoolID: "pool-single", Name: "Pool Single", WarningThresholdPct: 90, Actor: "tester"}); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	if _, err := svc.AttachDisk(ctx, "pool-single", "/dev/sdb", "tester"); err != nil {
		t.Fatalf("first disk attach failed: %v", err)
	}
	if _, err := svc.AttachDisk(ctx, "pool-single", "/dev/sdc", "tester"); !errors.Is(err, domain.ErrInvalidState) {
		t.Fatalf("expected strict flow to reject second disk, got %v", err)
	}
}

func TestStorageManagementService_ReserveWriteUsesMountedFreeSpace(t *testing.T) {
	t.Setenv("HOLO_STRICT_STORAGE_FLOW", "1")
	repo := memory.NewStoragePoolRepo()
	runner := &fakeStorageRunner{
		mountSource: "/dev/sdb",
		dfOutput:    "1B-blocks Used Avail\n100 90 10\n",
	}
	svc := NewStorageManagementService(repo, audit.NewMemoryWriter(), runner)
	ctx := context.Background()

	pool, err := domain.NewStoragePoolRuntime("pool-mounted", "Pool Mounted", 90)
	if err != nil {
		t.Fatalf("create pool runtime failed: %v", err)
	}
	if err := repo.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	if _, err := repo.AttachDisk(ctx, "pool-mounted", domain.StoragePoolDisk{DevicePath: "/dev/sdb", SizeBytes: 1000}); err != nil {
		t.Fatalf("attach disk failed: %v", err)
	}

	if _, _, err := svc.ReserveWrite(ctx, "pool-mounted", 20); !errors.Is(err, domain.ErrCapacityExceeded) {
		t.Fatalf("expected mounted free space capacity rejection, got %v", err)
	}
}

func TestStorageManagementService_MountSourceMustMatchDeviceExactly(t *testing.T) {
	runner := &fakeStorageRunner{
		output:      sampleLsblk,
		mountSource: "/dev/sdb1",
		fstype:      "xfs",
	}
	svc := NewStorageManagementService(memory.NewStoragePoolRepo(), audit.NewMemoryWriter(), runner)

	err := svc.mountPoolRootToDisk(context.Background(), "pool-exact", "/dev/sdb")
	if !errors.Is(err, domain.ErrInvalidState) {
		t.Fatalf("expected invalid state for mismatched source, got %v", err)
	}
	for _, command := range runner.commands {
		if strings.Contains(command, " chown ") {
			t.Fatalf("unexpected chown for mismatched source: %#v", runner.commands)
		}
	}
}

func TestStorageManagementService_UnmountSourceMustMatchExpectedDeviceExactly(t *testing.T) {
	runner := &fakeStorageRunner{mountSource: "/dev/sdb1"}
	svc := NewStorageManagementService(memory.NewStoragePoolRepo(), audit.NewMemoryWriter(), runner)

	err := svc.unmountPoolRoot(context.Background(), "pool-exact", "/dev/sdb")
	if !errors.Is(err, domain.ErrInvalidState) {
		t.Fatalf("expected invalid state for mismatched source, got %v", err)
	}
	for _, command := range runner.commands {
		if strings.Contains(command, " umount ") {
			t.Fatalf("unexpected umount for mismatched source: %#v", runner.commands)
		}
	}
}

func TestStorageManagementService_ReserveWriteWarningAndExhaustion(t *testing.T) {
	svc := newStorageSvcForTest()
	ctx := context.Background()

	if _, err := svc.CreatePool(ctx, CreateStoragePoolRequest{PoolID: "pool-2", Name: "Pool 2", WarningThresholdPct: 80, Actor: "tester"}); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	if _, err := svc.AttachDisk(ctx, "pool-2", "/dev/sdb", "tester"); err != nil {
		t.Fatalf("attach disk failed: %v", err)
	}

	snap, warning, err := svc.ReserveWrite(ctx, "pool-2", 900*1024*1024*1024)
	if err != nil {
		t.Fatalf("reserve write failed: %v", err)
	}
	if !warning {
		t.Fatalf("expected warning trigger at high usage")
	}
	if !snap.Warning {
		t.Fatalf("expected snapshot warning=true")
	}

	if _, _, err := svc.ReserveWrite(ctx, "pool-2", 10*1024*1024*1024*1024); err != domain.ErrCapacityExceeded {
		t.Fatalf("expected capacity exceeded, got %v", err)
	}
}

func TestStorageManagementService_DeletePoolReleasesAttachedDisk(t *testing.T) {
	svc := newStorageSvcForTest()
	ctx := context.Background()

	if _, err := svc.CreatePool(ctx, CreateStoragePoolRequest{PoolID: "pool-a", Name: "Pool A", WarningThresholdPct: 90, Actor: "tester"}); err != nil {
		t.Fatalf("create pool a failed: %v", err)
	}
	if _, err := svc.AttachDisk(ctx, "pool-a", "/dev/sdb", "tester"); err != nil {
		t.Fatalf("attach disk to pool a failed: %v", err)
	}
	if err := svc.DeletePool(ctx, "pool-a", "tester"); err != nil {
		t.Fatalf("delete pool a failed: %v", err)
	}

	if _, err := svc.CreatePool(ctx, CreateStoragePoolRequest{PoolID: "pool-b", Name: "Pool B", WarningThresholdPct: 90, Actor: "tester"}); err != nil {
		t.Fatalf("create pool b failed: %v", err)
	}
	if _, err := svc.AttachDisk(ctx, "pool-b", "/dev/sdb", "tester"); err != nil {
		t.Fatalf("expected released disk to attach to pool b, got %v", err)
	}
}

func TestStorageManagementService_DeletePoolReleasesWholeDiskXfsForReuse(t *testing.T) {
	repo := memory.NewStoragePoolRepo()
	runner := &fakeStorageRunner{
		output: sampleLsblkWholeDiskXfs,
		fstype: "xfs",
	}
	svc := NewStorageManagementService(repo, audit.NewMemoryWriter(), runner)
	ctx := context.Background()

	if _, err := svc.CreatePool(ctx, CreateStoragePoolRequest{PoolID: "pool-a", Name: "Pool A", WarningThresholdPct: 90, Actor: "tester"}); err != nil {
		t.Fatalf("create pool a failed: %v", err)
	}
	if _, err := svc.AttachDisk(ctx, "pool-a", "/dev/sdb", "tester"); err != nil {
		t.Fatalf("attach disk to pool a failed: %v", err)
	}
	if err := svc.DeletePool(ctx, "pool-a", "tester"); err != nil {
		t.Fatalf("delete pool a failed: %v", err)
	}

	if _, err := svc.CreatePool(ctx, CreateStoragePoolRequest{PoolID: "pool-b", Name: "Pool B", WarningThresholdPct: 90, Actor: "tester"}); err != nil {
		t.Fatalf("create pool b failed: %v", err)
	}
	if _, err := svc.AttachDisk(ctx, "pool-b", "/dev/sdb", "tester"); err != nil {
		t.Fatalf("expected released whole-disk xfs to attach to pool b, got %v", err)
	}
}

func TestStorageManagementService_DiscoverDisksRunnerFailure(t *testing.T) {
	repo := memory.NewStoragePoolRepo()
	svc := NewStorageManagementService(repo, audit.NewMemoryWriter(), &fakeStorageRunner{err: errors.New("lsblk unavailable")})
	if _, err := svc.DiscoverDisks(context.Background()); err == nil {
		t.Fatalf("expected discover failure")
	}
}
