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
  portal: string;
  state: "creating" | "ready" | "failed" | "disabled" | string;
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
  desiredIqns: string[];
  mountedIqns: string[];
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

export interface InitiatorRule {
  ruleId: string;
  publicationId: string;
  initiator: string;
  permission: "allow" | "deny";
  priority: number;
  createdAt: string;
}

export interface AuthorizationDecision {
  publicationId: string;
  initiator: string;
  decision: "allow" | "deny";
  reason: string;
  matchedRuleId?: string;
  evaluatedAt: string;
}

export interface DiscoverableTarget {
  publicationId: string;
  targetIqn: string;
  portal: string;
  state: string;
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
