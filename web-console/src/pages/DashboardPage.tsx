import { type CSSProperties, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  CassetteTape,
  Cpu,
  DatabaseBackup,
  HardDrive,
  LibraryBig,
  MemoryStick,
  Network,
  RadioTower,
  Server,
  Waypoints,
} from "lucide-react";
import { api } from "../services/api";
import { formatBytes } from "../utils/format";
import type { HealthSummary, StoragePoolRuntime, SystemOverview, VirtualCartridge, VirtualLibrary } from "../services/types";

function formatPercent(value: number): string {
  if (!Number.isFinite(value) || value < 0) {
    return "0%";
  }
  return `${Math.round(value)}%`;
}

function percentValue(value: number): number {
  if (!Number.isFinite(value)) {
    return 0;
  }
  return Math.max(0, Math.min(100, value));
}

function formatUptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) {
    return "0m";
  }
  const totalMinutes = Math.floor(seconds / 60);
  const days = Math.floor(totalMinutes / (24 * 60));
  const hours = Math.floor((totalMinutes % (24 * 60)) / 60);
  const minutes = totalMinutes % 60;
  if (days > 0) {
    return `${days}d ${hours}h`;
  }
  if (hours > 0) {
    return `${hours}h ${minutes}m`;
  }
  return `${minutes}m`;
}

export function DashboardPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();

  const [health, setHealth] = useState<HealthSummary | null>(null);
  const [overview, setOverview] = useState<SystemOverview | null>(null);
  const [pools, setPools] = useState<StoragePoolRuntime[]>([]);
  const [libraries, setLibraries] = useState<VirtualLibrary[]>([]);
  const [cartridges, setCartridges] = useState<VirtualCartridge[]>([]);
  const [publicationCount, setPublicationCount] = useState(0);

  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [partialNotice, setPartialNotice] = useState("");

  useEffect(() => {
    let cancelled = false;
    (async () => {
      setLoading(true);
      setError("");
      setPartialNotice("");

      const results = await Promise.allSettled([
        api.ops.health(),
        api.ops.systemOverview(),
        api.storage.listPools(),
        api.resources.listLibraries(),
        api.resources.listCartridges(),
        api.targets.listPublications(),
      ]);

      if (cancelled) {
        return;
      }

      const requiredDataSourceCount = results.length;
      const requiredFailed: string[] = [];

      if (results[0].status === "fulfilled") {
        setHealth(results[0].value);
      } else {
        requiredFailed.push("health");
      }

      if (results[1].status === "fulfilled") {
        setOverview(results[1].value);
      } else {
        requiredFailed.push("system");
      }

      if (results[2].status === "fulfilled") {
        setPools(results[2].value);
      } else {
        requiredFailed.push("storage");
      }

      if (results[3].status === "fulfilled") {
        setLibraries(results[3].value);
      } else {
        requiredFailed.push("libraries");
      }

      if (results[4].status === "fulfilled") {
        setCartridges(results[4].value);
      } else {
        requiredFailed.push("cartridges");
      }

      if (results[5].status === "fulfilled") {
        setPublicationCount(results[5].value.length);
      } else {
        requiredFailed.push("targets");
      }

      if (requiredFailed.length === requiredDataSourceCount) {
        setError(t("dashboard.noData"));
      } else if (requiredFailed.length > 0) {
        setPartialNotice(t("dashboard.partialData", { count: requiredFailed.length }));
      }

      setLoading(false);
    })();

    return () => {
      cancelled = true;
    };
  }, [t]);

  const storageTotals = useMemo(() => {
    let totalBytes = 0;
    let usedBytes = 0;
    let warningPools = 0;
    for (const pool of pools) {
      totalBytes += pool.capacity.totalBytes;
      usedBytes += pool.capacity.usedBytes;
      if (pool.capacity.warning || pool.capacity.exhausted) {
        warningPools += 1;
      }
    }
    return { totalBytes, usedBytes, warningPools };
  }, [pools]);

  const memoryUsage = useMemo(() => {
    if (!overview || overview.memoryTotalBytes <= 0) {
      return 0;
    }
    const used = overview.memoryTotalBytes - overview.memoryAvailableBytes;
    return Math.max(0, (used / overview.memoryTotalBytes) * 100);
  }, [overview]);

  const storageUsedPercent = useMemo(() => {
    if (storageTotals.totalBytes <= 0) {
      return 0;
    }
    return percentValue((storageTotals.usedBytes / storageTotals.totalBytes) * 100);
  }, [storageTotals.totalBytes, storageTotals.usedBytes]);

  const alertCount =
    storageTotals.warningPools +
    (health && health.status.toLowerCase() !== "healthy" ? 1 : 0);

  const healthLabel = health?.status || t("dashboard.unknown");
  const healthy = healthLabel.toLowerCase() === "healthy";
  const collectedAt = overview?.collectedAt ? new Date(overview.collectedAt).toLocaleString() : "-";

  const topPools = useMemo(
    () => [...pools].sort((a, b) => b.capacity.usedPercent - a.capacity.usedPercent).slice(0, 4),
    [pools]
  );

  const libraryRows = useMemo(
    () =>
      libraries.map((library) => {
        const cartridgeCount = cartridges.filter((item) => item.libraryId === library.libraryId).length;
        const slotCount = Math.max(library.slotCount || 0, cartridgeCount, 1);
        return {
          library,
          cartridgeCount,
          slotCount,
          fillPercent: percentValue((cartridgeCount / slotCount) * 100),
        };
      }),
    [cartridges, libraries]
  );

  const inventoryTotals = useMemo(() => {
    const slotCount = libraryRows.reduce((sum, row) => sum + row.slotCount, 0);
    if (slotCount <= 0) {
      return { cartridgeCount: 0, fillPercent: 0, slotCount: 0 };
    }
    const cartridgeCount = libraryRows.reduce((sum, row) => sum + row.cartridgeCount, 0);
    return {
      cartridgeCount,
      fillPercent: percentValue((cartridgeCount / slotCount) * 100),
      slotCount,
    };
  }, [libraryRows]);

  const loadSamples = [
    overview?.cpuLoad15m || 0,
    overview?.cpuLoad5m || 0,
    overview?.cpuLoad1m || 0,
  ];
  const loadMax = Math.max(...loadSamples, 1);
  const loadPoints = loadSamples
    .map((value, index) => {
      const x = 10 + index * 45;
      const y = 72 - (value / loadMax) * 48;
      return `${x},${y}`;
    })
    .join(" ");

  return (
    <section className="dashboard-page">
      <div className="page-header dashboard-header">
        <div>
          <h1 className="page-title">{t("dashboard.title")}</h1>
        </div>
        <div className="dashboard-collected">
          <Waypoints size={14} />
          {t("dashboard.updatedAt", { time: collectedAt })}
        </div>
      </div>

      {error ? <p className="notice notice-error">{error}</p> : null}
      {!error && partialNotice ? <p className="notice">{partialNotice}</p> : null}
      {loading ? <p className="notice">{t("common.loading")}</p> : null}

      <div className="dashboard-hero">
        <div className="dashboard-status-panel">
          <div className="dashboard-status-topline">
            <span className={`status-badge ${healthy ? "status-ok" : "status-warn"}`}>
              <Activity size={12} />
              {healthLabel}
            </span>
            <span className={`status-badge ${alertCount > 0 ? "status-warn" : "status-ok"}`}>
              <AlertTriangle size={12} />
              {t("dashboard.alerts")}: {alertCount}
            </span>
          </div>
          <div className="dashboard-command-title">{overview?.hostname || t("dashboard.unknown")}</div>
          <div className="meta-pill">{t("dashboard.uptime")} {formatUptime(overview?.uptimeSeconds || 0)}</div>
          <div className="dashboard-command-metrics">
            <div>
              <Server size={16} />
              <strong>{overview ? overview.cpuLoad1m.toFixed(2) : "0.00"}</strong>
              <span>{t("dashboard.cpuLoad")}</span>
            </div>
            <div>
              <RadioTower size={16} />
              <strong>{overview?.iscsiSessionCount || 0}</strong>
              <span>{t("dashboard.connectedSystems")}</span>
            </div>
            <div>
              <DatabaseBackup size={16} />
              <strong>{publicationCount}</strong>
              <span>{t("dashboard.publications")}</span>
            </div>
          </div>
        </div>

        <div className="dashboard-runtime-panel">
          <div className="runtime-tile">
            <Cpu size={18} />
            <span>{t("dashboard.cpuLoad")}</span>
            <strong>{overview ? overview.cpuLoad1m.toFixed(2) : "0.00"}</strong>
            <svg viewBox="0 0 110 80" aria-hidden="true" className="runtime-sparkline">
              <polyline points={loadPoints} />
            </svg>
          </div>
          <div className="runtime-tile">
            <MemoryStick size={18} />
            <span>{t("dashboard.memoryUsage")}</span>
            <strong>{formatPercent(memoryUsage)}</strong>
            <div className="runtime-meter"><span style={{ width: `${percentValue(memoryUsage)}%` }} /></div>
          </div>
          <div className="runtime-tile">
            <Network size={18} />
            <span>{t("dashboard.networkRx")}</span>
            <strong>{formatBytes(overview?.networkRxBytes || 0)}</strong>
            <small>{t("dashboard.networkTx")}: {formatBytes(overview?.networkTxBytes || 0)}</small>
          </div>
        </div>
      </div>

      <div className="dashboard-flow" aria-label={t("dashboard.resourceFlow")}>
        <div className="flow-node flow-node-storage">
          <HardDrive size={18} />
          <span>{t("dashboard.pools")}</span>
          <strong>{pools.length}</strong>
          <small>{formatBytes(storageTotals.totalBytes)}</small>
        </div>
        <div className="flow-link" />
        <div className="flow-node flow-node-media">
          <CassetteTape size={18} />
          <span>{t("dashboard.cartridges")}</span>
          <strong>{cartridges.length}</strong>
          <small>{formatBytes(storageTotals.usedBytes)}</small>
        </div>
        <div className="flow-link" />
        <div className="flow-node flow-node-library">
          <LibraryBig size={18} />
          <span>{t("dashboard.libraries")}</span>
          <strong>{libraries.length}</strong>
          <small>{t("dashboard.libraryCount", { count: libraries.length })}</small>
        </div>
        <div className="flow-link" />
        <div className="flow-node flow-node-target">
          <RadioTower size={18} />
          <span>{t("dashboard.connectedSystems")}</span>
          <strong>{overview?.iscsiSessionCount || 0}</strong>
          <small>{t("dashboard.publishedTargets", { count: publicationCount })}</small>
        </div>
      </div>

      <div className="dashboard-main-grid">
        <section className="panel dashboard-capacity-panel">
          <div className="panel-heading">
            <div>
              <h3>{t("dashboard.storageUsage")}</h3>
              <p>{topPools.length === 0 ? t("dashboard.storageStepTitle") : `${formatBytes(storageTotals.usedBytes)} / ${formatBytes(storageTotals.totalBytes)}`}</p>
            </div>
            {topPools.length > 0 ? (
              <div
                className="capacity-ring"
                style={{ "--ring-value": `${storageUsedPercent * 3.6}deg` } as CSSProperties}
                aria-label={formatPercent(storageUsedPercent)}
              >
                <strong>{formatPercent(storageUsedPercent)}</strong>
              </div>
            ) : null}
          </div>

          <div className="pool-stack">
            {topPools.length === 0 ? (
              <div className="onboarding-empty">
                <div className="onboarding-step">Step 1</div>
                <div>
                  <strong>{t("dashboard.storageStepTitle")}</strong>
                  <span>{t("dashboard.storageStepBody")}</span>
                </div>
                <button className="btn btn-primary" type="button" onClick={() => navigate("/storage")}>
                  <HardDrive size={14} />
                  {t("dashboard.storageStepAction")}
                </button>
              </div>
            ) : null}
            {topPools.map((pool) => (
              <div className="pool-row" key={pool.poolId}>
                <div>
                  <strong>{pool.name}</strong>
                  <span>{pool.poolId}</span>
                </div>
                <div className="pool-row-meter">
                  <span style={{ width: `${percentValue(pool.capacity.usedPercent)}%` }} />
                </div>
                <em>{formatPercent(pool.capacity.usedPercent)}</em>
              </div>
            ))}
          </div>
        </section>

        <section className="panel dashboard-inventory-panel">
          <div className="panel-heading">
            <div>
              <h3>{t("dashboard.vtlInventory")}</h3>
            </div>
            {libraryRows.length > 0 ? (
              <div className="inventory-occupancy">
                <span>{t("dashboard.slotOccupancy")}</span>
                <strong>{formatPercent(inventoryTotals.fillPercent)}</strong>
                <em>{t("dashboard.occupiedSlotsRatio", { used: inventoryTotals.cartridgeCount, total: inventoryTotals.slotCount })}</em>
              </div>
            ) : null}
          </div>

          <div className="library-stack">
            {libraryRows.length === 0 ? (
              <div className="onboarding-empty">
                <div className="onboarding-step">Step 2</div>
                <div>
                  <strong>{t("dashboard.vtlStepTitle")}</strong>
                  <span>{t("dashboard.vtlStepBody")}</span>
                </div>
                <button className="btn btn-primary" type="button" onClick={() => navigate("/resources")}>
                  <LibraryBig size={14} />
                  {t("dashboard.vtlStepAction")}
                </button>
              </div>
            ) : null}
            {libraryRows.map(({ library, cartridgeCount, slotCount, fillPercent }) => (
              <button
                className="library-strip"
                key={library.libraryId}
                type="button"
                onClick={() => navigate(`/resources/${library.libraryId}/manage`)}
              >
                <div className="library-strip-main">
                  <LibraryBig size={18} />
                  <div>
                    <strong>{library.name}</strong>
                    <span>{library.vendor || "-"} / {library.libraryType || "-"}</span>
                  </div>
                </div>
                <div className="library-strip-topology" aria-hidden="true">
                  {Array.from({ length: Math.min(slotCount, 16) }, (_, index) => (
                    <span key={`${library.libraryId}-${index}`} className={index < cartridgeCount ? "filled" : ""} />
                  ))}
                </div>
                <div className="library-strip-meta">
                  <span>{library.driveCount || 0} {t("resources.driveCount")}</span>
                  <span>{cartridgeCount}/{slotCount} {t("resources.slotCount")}</span>
                  <span>{formatPercent(fillPercent)}</span>
                </div>
              </button>
            ))}
          </div>
        </section>
      </div>

    </section>
  );
}
