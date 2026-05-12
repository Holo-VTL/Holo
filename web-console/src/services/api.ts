import type {
  ApiError,
  AuditEvent,
  AuthorizationDecision,
  CDBTraceStatus,
  DiscoverableTarget,
  HealthSummary,
  InitiatorRule,
  StorageManagedDisk,
  StoragePoolCapacitySnapshot,
  StoragePoolRuntime,
  SystemOverview,
  TargetPublication,
  ValidationRun,
  VirtualCartridge,
  VirtualDrive,
  VirtualLibrary,
} from "./types";

type RequestOptions = {
  method?: "GET" | "POST" | "DELETE";
  body?: unknown;
};

export const HOLO_API_KEY_SESSION_KEY = "holo.apiKey";

type RuntimeConfig = {
  apiBaseUrl: string;
};

const defaultRuntimeConfig: RuntimeConfig = {
  apiBaseUrl: "",
};

let runtimeConfigPromise: Promise<RuntimeConfig> | undefined;

function isApiError(value: unknown): value is ApiError {
  return typeof value === "object" && value !== null && "status" in value;
}

function configPath(): string {
  const base = import.meta.env.BASE_URL || "/";
  const normalized = base.endsWith("/") ? base : `${base}/`;
  return `${normalized}config.json`;
}

function normalizeApiBaseUrl(raw: unknown): string {
  if (typeof raw !== "string") {
    return "";
  }
  return raw.trim().replace(/\/+$/, "");
}

async function loadRuntimeConfig(): Promise<RuntimeConfig> {
  if (!runtimeConfigPromise) {
    runtimeConfigPromise = fetch(configPath(), {
      method: "GET",
      headers: { Accept: "application/json" },
      cache: "no-store",
    })
      .then(async (response) => {
        if (!response.ok) {
          return defaultRuntimeConfig;
        }
        const payload = (await response.json()) as Partial<RuntimeConfig>;
        return {
          apiBaseUrl: normalizeApiBaseUrl(payload.apiBaseUrl),
        };
      })
      .catch(() => defaultRuntimeConfig);
  }
  return runtimeConfigPromise;
}

async function apiURL(path: string): Promise<string> {
  const cfg = await loadRuntimeConfig();
  const normalizedPath = path.startsWith("/") ? path : `/${path}`;
  if (cfg.apiBaseUrl === "") {
    return normalizedPath;
  }
  return `${cfg.apiBaseUrl}${normalizedPath}`;
}

export function resetRuntimeConfigForTest() {
  runtimeConfigPromise = undefined;
}

function getStoredAPIKey(): string {
  if (typeof window === "undefined" || !window.sessionStorage) {
    return "";
  }
  return (window.sessionStorage.getItem(HOLO_API_KEY_SESSION_KEY) || "").trim();
}

export function setHoloAPIKeyForSession(apiKey: string) {
  if (typeof window === "undefined" || !window.sessionStorage) {
    return;
  }
  const trimmed = apiKey.trim();
  if (trimmed === "") {
    window.sessionStorage.removeItem(HOLO_API_KEY_SESSION_KEY);
    return;
  }
  window.sessionStorage.setItem(HOLO_API_KEY_SESSION_KEY, trimmed);
}

export function clearHoloAPIKeyForSession() {
  if (typeof window !== "undefined" && window.sessionStorage) {
    window.sessionStorage.removeItem(HOLO_API_KEY_SESSION_KEY);
  }
}

export function hasHoloAPIKeyForSession(): boolean {
  return getStoredAPIKey() !== "";
}

function applyAuthHeaders(headers: Record<string, string>) {
  const apiKey = getStoredAPIKey();
  if (apiKey !== "") {
    headers["X-HOLO-API-Key"] = apiKey;
  }
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const headers: Record<string, string> = {
    Accept: "application/json",
  };
  if (options.body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  applyAuthHeaders(headers);

  const response = await fetch(await apiURL(path), {
    method: options.method || "GET",
    headers,
    body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
  });

  if (response.status === 204) {
    return undefined as T;
  }

  const contentType = response.headers.get("content-type") || "";
  let payload: unknown = undefined;
  if (contentType.includes("application/json")) {
    payload = await response.json();
  } else {
    payload = await response.text();
  }

  if (!response.ok) {
    const err = new Error(response.statusText) as ApiError;
    err.status = response.status;
    if (typeof payload === "string") {
      err.detail = payload;
      err.message = payload;
    } else if (payload && typeof payload === "object" && "error" in (payload as Record<string, unknown>)) {
      err.message = String((payload as Record<string, unknown>).error);
      err.detail = JSON.stringify(payload);
    } else {
      err.message = `HTTP ${response.status}`;
      err.detail = typeof payload === "undefined" ? "" : JSON.stringify(payload);
    }
    throw err;
  }

  return payload as T;
}

async function download(path: string): Promise<{ blob: Blob; filename: string }> {
  const headers: Record<string, string> = {};
  applyAuthHeaders(headers);
  const response = await fetch(await apiURL(path), {
    method: "GET",
    headers,
  });
  if (!response.ok) {
    const err = new Error(`HTTP ${response.status}`) as ApiError;
    err.status = response.status;
    err.detail = await response.text();
    throw err;
  }

  const disposition = response.headers.get("content-disposition") || "";
  const filenameMatch = /filename="?([^";]+)"?/i.exec(disposition);
  return {
    blob: await response.blob(),
    filename: filenameMatch?.[1] || "holo-support-bundle.zip",
  };
}

export const api = {
  ops: {
    health: async () => {
      const response = await fetch(await apiURL("/healthz"), {
        method: "GET",
        headers: { Accept: "application/json" },
      });
      const payload = (await response.json()) as HealthSummary;
      if (!response.ok && response.status !== 503) {
        throw new Error(`HTTP ${response.status}`);
      }
      return payload;
    },
    systemOverview: () => request<SystemOverview>("/v1/system/overview"),
    auditEvents: () => request<AuditEvent[]>("/v1/audit/events"),
    cdbTrace: () => request<CDBTraceStatus>("/v1/ops/cdb-trace"),
    setCDBTrace: (enabled: boolean) => request<CDBTraceStatus>("/v1/ops/cdb-trace", { method: "POST", body: { enabled } }),
    supportBundle: () => download("/v1/support/bundle"),
  },
  storage: {
    discoverDisks: async () => {
      const result = await request<{ disks: StorageManagedDisk[] }>("/v1/storage/disks/discovery");
      return result.disks;
    },
    listPools: () => request<StoragePoolRuntime[]>("/v1/storage/pools"),
    createPool: (body: { poolId: string; name: string; warningThresholdPct?: number; actor?: string }) =>
      request<StoragePoolRuntime>("/v1/storage/pools", { method: "POST", body }),
    deletePool: async (poolId: string, actor = "web-console") => {
      const target = `/v1/storage/pools/${encodeURIComponent(poolId)}?actor=${encodeURIComponent(actor)}`;
      const postDeleteTarget = `/v1/storage/pools/${encodeURIComponent(poolId)}/delete?actor=${encodeURIComponent(actor)}`;
      try {
        return await request<void>(target, { method: "DELETE" });
      } catch (err) {
        if (isApiError(err) && err.status === 405) {
          return request<void>(postDeleteTarget, {
            method: "POST",
          });
        }
        if (isApiError(err) && err.status === 400) {
          const pool = await request<StoragePoolRuntime>(`/v1/storage/pools/${encodeURIComponent(poolId)}`);
          if (pool.capacity.usedBytes === 0 && pool.disks.length > 0) {
            for (const disk of pool.disks) {
              await request<StoragePoolRuntime>(`/v1/storage/pools/${encodeURIComponent(poolId)}/disks/detach`, {
                method: "POST",
                body: { devicePath: disk.devicePath, actor },
              });
            }
            try {
              return await request<void>(target, { method: "DELETE" });
            } catch (retryErr) {
              if (isApiError(retryErr) && retryErr.status === 405) {
                return request<void>(postDeleteTarget, { method: "POST" });
              }
              throw retryErr;
            }
          }
        }
        throw err;
      }
    },
    attachDisk: (poolId: string, body: { devicePath: string; actor?: string }) =>
      request<StoragePoolRuntime>(`/v1/storage/pools/${encodeURIComponent(poolId)}/disks/attach`, {
        method: "POST",
        body,
      }),
    detachDisk: (poolId: string, body: { devicePath: string; actor?: string }) =>
      request<StoragePoolRuntime>(`/v1/storage/pools/${encodeURIComponent(poolId)}/disks/detach`, {
        method: "POST",
        body,
      }),
    poolCapacity: (poolId: string) =>
      request<StoragePoolCapacitySnapshot>(`/v1/storage/pools/${encodeURIComponent(poolId)}/capacity`),
  },
  resources: {
    listLibraries: () => request<VirtualLibrary[]>("/v1/libraries"),
    createLibrary: (body: {
      libraryId: string;
      name: string;
      vendor?: string;
      libraryType?: string;
      driveType?: string;
      driveCount?: number;
      driveStartAddress?: number;
      slotCount?: number;
      slotStartAddress?: number;
      iePortCount?: number;
      ieStartAddress?: number;
      compressionEnabled?: boolean;
      dedupEnabled?: boolean;
    }) => request<VirtualLibrary>("/v1/libraries", { method: "POST", body }),
    deleteLibrary: (libraryId: string) =>
      request<void>(`/v1/libraries/${encodeURIComponent(libraryId)}/delete`, { method: "POST" }),
    listDrives: () => request<VirtualDrive[]>("/v1/drives"),
    createDrive: (body: { driveId: string; libraryId: string; slot: number }) =>
      request<VirtualDrive>("/v1/drives", { method: "POST", body }),
    deleteDrive: (driveId: string) =>
      request<void>(`/v1/drives/${encodeURIComponent(driveId)}/delete`, { method: "POST" }),
    loadCartridge: (driveId: string, body: { cartridgeId: string; actor?: string }) =>
      request<VirtualDrive>(`/v1/drives/${encodeURIComponent(driveId)}/load`, { method: "POST", body }),
    unloadDrive: (driveId: string, actor = "web-console") =>
      request<VirtualDrive>(`/v1/drives/${encodeURIComponent(driveId)}/unload`, {
        method: "POST",
        body: { actor },
      }),
    listCartridges: () => request<VirtualCartridge[]>("/v1/cartridges"),
    createCartridge: (body: {
      cartridgeId?: string;
      poolId: string;
      libraryId: string;
      barcode?: string;
      capacityBytes: number;
      ltoGeneration?: number;
      mediaType?: string;
    }) => request<VirtualCartridge>("/v1/cartridges", { method: "POST", body }),
    deleteCartridge: (cartridgeId: string) =>
      request<void>(`/v1/cartridges/${encodeURIComponent(cartridgeId)}/delete`, { method: "POST" }),
    eraseCartridge: (cartridgeId: string, body: { mode: "short" | "long"; actor?: string }) =>
      request<VirtualCartridge>(`/v1/cartridges/${encodeURIComponent(cartridgeId)}/erase`, {
        method: "POST",
        body,
      }),
    exportCartridge: (cartridgeId: string, actor = "web-console") =>
      request<VirtualCartridge>(`/v1/cartridges/${encodeURIComponent(cartridgeId)}/export`, {
        method: "POST",
        body: { actor },
      }),
    importCartridge: (cartridgeId: string, actor = "web-console") =>
      request<VirtualCartridge>(`/v1/cartridges/${encodeURIComponent(cartridgeId)}/import`, {
        method: "POST",
        body: { actor },
      }),
  },
  targets: {
    listPublications: () => request<TargetPublication[]>("/v1/targets/publications"),
    createPublication: (body: {
      libraryId: string;
      driveId: string;
      cartridgeId: string;
      targetIqn: string;
      deviceRole?: string;
      deviceProfile?: string;
      actor?: string;
    }) => request<TargetPublication>("/v1/targets/publications", { method: "POST", body }),
    unpublish: (publicationId: string, actor = "web-console") =>
      request<TargetPublication>(
        `/v1/targets/publications/${encodeURIComponent(publicationId)}/delete?actor=${encodeURIComponent(actor)}`,
        { method: "POST" }
      ),
    rollback: (publicationId: string, actor = "web-console") =>
      request<TargetPublication>(
        `/v1/targets/publications/${encodeURIComponent(publicationId)}/rollback?actor=${encodeURIComponent(actor)}`,
        { method: "POST" }
      ),
    listValidationRuns: (publicationId: string) =>
      request<ValidationRun[]>(`/v1/targets/publications/${encodeURIComponent(publicationId)}/validation-runs`),
    startValidationRun: (
      publicationId: string,
      body: { mode?: "fixed" | "empty"; bytes?: number; pattern?: string },
      actor = "web-console"
    ) =>
      request<ValidationRun>(
        `/v1/targets/publications/${encodeURIComponent(publicationId)}/validation-runs?actor=${encodeURIComponent(actor)}`,
        { method: "POST", body }
      ),
    listAccessRules: async (publicationId: string) => {
      const result = await request<{ publicationId: string; rules: InitiatorRule[] }>(
        `/v1/targets/publications/${encodeURIComponent(publicationId)}/access-rules`
      );
      return result.rules;
    },
    replaceAccessRules: (publicationId: string, body: { actor?: string; rules: Array<Partial<InitiatorRule>> }) =>
      request(`/v1/targets/publications/${encodeURIComponent(publicationId)}/access-rules`, {
        method: "POST",
        body,
      }),
    authorize: (publicationId: string, body: { initiator: string; actor?: string }) =>
      request<AuthorizationDecision>(`/v1/targets/publications/${encodeURIComponent(publicationId)}/authorize`, {
        method: "POST",
        body,
      }),
    rollbackAccess: (publicationId: string, body: { actor?: string }) =>
      request(`/v1/targets/publications/${encodeURIComponent(publicationId)}/access-rollback`, {
        method: "POST",
        body,
      }),
    visible: async (params: { initiator: string; actor?: string }) => {
      const query = new URLSearchParams({ initiator: params.initiator, actor: params.actor || "web-console" });
      const result = await request<{ initiator: string; publications: TargetPublication[] }>(
        `/v1/targets/visible?${query.toString()}`
      );
      return result.publications;
    },
    discovery: async (params: { initiator: string; actor?: string; portal?: string }) => {
      const query = new URLSearchParams({ initiator: params.initiator, actor: params.actor || "web-console" });
      if (params.portal) {
        query.set("portal", params.portal);
      }
      const result = await request<{ initiator: string; portal?: string; targets: DiscoverableTarget[] }>(
        `/v1/targets/discovery?${query.toString()}`
      );
      return result.targets;
    },
  },
  policy: {
    createAccessPolicy: (body: {
      policyId: string;
      scope: "global" | "library" | "drive";
      subject: string;
      permission: "allow" | "deny";
      effectiveFrom: string;
      effectiveTo?: string;
    }) => request("/v1/access-policies", { method: "POST", body }),
    createRetentionPolicy: (body: {
      retentionId: string;
      cartridgeId: string;
      mode: "worm" | "governance";
      lockUntil: string;
      createdBy: string;
    }) => request("/v1/retention-policies", { method: "POST", body }),
  },
};
