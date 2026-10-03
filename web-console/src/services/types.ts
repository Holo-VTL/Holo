export interface ApiError extends Error {
  status: number;
  detail?: string;
}

export type DiskAvailability = "available" | "unavailable";

export interface StorageManagedDisk {
  devicePath: string;
  sizeBytes: number;
  vendor?: string;
  model?: string;
  serial?: string;
  availability: DiskAvailability;
  unavailableReason?: string;
  poolId?: string;
}

export interface StoragePoolDisk {
  devicePath: string;
  sizeBytes: number;
  attachedAt: string;
}

export interface StoragePoolCapacitySnapshot {
  totalBytes: number;
  usedBytes: number;
  freeBytes: number;
  usedPercent: number;
  warning: boolean;
  exhausted: boolean;
  warningThresholdPct: number;
}

export interface StoragePoolRuntime {
  poolId: string;
  name: string;
  status: "active" | "degraded";
  warningThresholdPct: number;
  disks: StoragePoolDisk[];
  capacity: StoragePoolCapacitySnapshot;
  createdAt: string;
  updatedAt: string;
}

export interface VirtualLibrary {
  libraryId: string;
  name: string;
  status: string;
  vendor?: string;
  libraryType?: string;
  driveType?: string;
  driveCount?: number;
  driveStartAddress?: number;
  slotCount?: number;
  slotStartAddress?: number;
  iePortCount?: number;
  ieStartAddress?: number;
  iqn?: string;
  compressionEnabled: boolean;
  dedupEnabled: boolean;
  createdAt: string;
  updatedAt: string;
}

export interface VirtualDrive {
  driveId: string;
  libraryId: string;
  slot: number;
  iqn?: string;
  status?: string;
  mountState?: "empty" | "loaded" | "busy" | "error" | string;
  mountedCartridgeId?: string;
  loadedCartridgeId?: string;
  createdAt: string;
  updatedAt: string;
}

export interface VirtualCartridge {
  cartridgeId: string;
  poolId: string;
  libraryId: string;
  barcode: string;
  capacityBytes: number;
  usedBytes: number;
  lifecycleState: string;
  retentionState: string;
  currentElementAddress?: number;
  assignedSlotAddress?: number;
  createdAt: string;
  updatedAt: string;
}

export interface TargetPublication {
  publicationId: string;
  poolId: string;
  libraryId: string;
  driveId: string;
  cartridgeId: string;
  targetIqn: string;
  deviceRole: "drive" | "changer" | string;
  deviceProfile?: string;
  driveProfile?: string;
  portal: string;
  state: "creating" | "ready" | "failed" | "disabled" | string;
  securityEnforcement?: "unprotected" | "simulated" | "enforcing" | "blocked" | "offline" | string;
  lastError?: string;
  compressionEnabled: boolean;
  dedupEnabled: boolean;
  connectedHosts?: ConnectedHostsSummary;
  createdAt: string;
  updatedAt: string;
}

export interface ConnectedHostsSummary {
  available: boolean;
  hostCount: number;
  sessionCount: number;
  initiators: string[];
  lastError?: string;
}

export interface LocalMountStatus {
  enabled: boolean;
  state: "disabled" | "connecting" | "connected" | "partial" | "failed" | "disconnecting";
  desiredDeviceCount: number;
  connectedDeviceCount: number;
  residualDeviceCount: number;
  devices: Array<{
    deviceKey: string;
    kind: "changer" | "drive";
    libraryId: string;
    driveId?: string;
    displayName: string;
    state: "pending" | "connected" | "not_ready" | "failed" | "removing" | "residual";
    observedPaths: string[];
    reasonCode?: string;
    message?: string;
  }>;
  desiredIqns?: string[];
  mountedIqns?: string[];
  skippedTargets?: string[];
  lastSyncAt?: string;
  lastError?: string;
}

export interface ValidationRun {
  validationId: string;
  publicationId: string;
  scenario: string;
  status: string;
  mode: string;
  bytesWritten: number;
  bytesRead: number;
  writeDigest?: string;
  readDigest?: string;
  evidencePath?: string;
  startedAt: string;
  finishedAt?: string;
}

export interface DiscoverableTarget {
  publicationId: string;
  targetIqn: string;
  portal: string;
  state: string;
}

export type ISCSIAuthMode = "none" | "chap" | "mutual_chap";

export interface ISCSIAuthenticationPolicy {
  mode: ISCSIAuthMode;
  credentialId?: string;
  initiators?: string[];
  restrictInitiators?: boolean;
}

export interface ISCSISecurityOrigin {
  scope: string;
  ownerId?: string;
  generation?: number;
}

export interface ResolvedISCSISecurity {
  auth: ISCSIAuthenticationPolicy;
  authSource: ISCSISecurityOrigin;
  effectiveRevision: string;
}

export interface ISCSISecurityBinding {
  scope: "library" | "drive" | "target";
  ownerId: string;
  libraryId?: string;
  driveId?: string;
  targetIqn?: string;
  deviceRole?: string;
  auth?: ISCSIAuthenticationPolicy | null;
  generation: number;
  administrativeOffline?: boolean;
}

export interface ISCSISecurityTargetView {
  binding: ISCSISecurityBinding;
  resolved: ResolvedISCSISecurity;
}

export interface ISCSISecurityImpact {
  targetIqn: string;
  deviceRole: string;
  administrativeOffline: boolean;
  absent: boolean;
  busy: boolean;
  changed: boolean;
  before: ResolvedISCSISecurity;
  after: ResolvedISCSISecurity;
}

export interface ISCSICredentialMetadata {
  credentialId: string;
  label: string;
  username: string;
  mutualUsername?: string;
  version: number;
  createdAt: string;
  secretPresent: boolean;
}

export interface AuditEvent {
  eventId: string;
  actor: string;
  action: string;
  objectType: string;
  objectId: string;
  result: string;
  details?: Record<string, unknown>;
  occurredAt: string;
}

export interface HealthSummary {
  status: "healthy" | "degraded" | string;
  components: Array<{
    name: string;
    status: "ok" | "down" | "unknown" | "healthy" | string;
    message?: string;
  }>;
}

export interface SystemOverview {
  hostname: string;
  uptimeSeconds: number;
  cpuLoad1m: number;
  cpuLoad5m: number;
  cpuLoad15m: number;
  memoryTotalBytes: number;
  memoryAvailableBytes: number;
  networkRxBytes: number;
  networkTxBytes: number;
  iscsiSessionCount: number;
  collectedAt: string;
}

export interface CDBTraceStatus {
  enabled: boolean;
  stateFile: string;
}
