import { FormEvent, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Plus, RefreshCw, Trash2 } from "lucide-react";
import { api } from "../services/api";
import { useToast } from "../components/Toast";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { SelectInput } from "../components/SelectInput";
import { formatBytes } from "../utils/format";
import type { StorageManagedDisk, StoragePoolRuntime } from "../services/types";

function nextAutoPoolId(pools: StoragePoolRuntime[]): string {
  const used = new Set(pools.map((pool) => pool.poolId));
  let idx = 1;
  while (used.has(`pool${idx}`)) {
    idx += 1;
  }
  return `pool${idx}`;
}

function diskAvailabilityLabel(disk: StorageManagedDisk, t: (key: string) => string): string {
  if (disk.poolId) {
    return t("storage.allocated");
  }
  if (disk.availability === "available") {
    return t("storage.available");
  }
  return t("storage.unavailable");
}

export function StoragePage() {
  const { t } = useTranslation();
  const { push } = useToast();
  const [disks, setDisks] = useState<StorageManagedDisk[]>([]);
  const [pools, setPools] = useState<StoragePoolRuntime[]>([]);
  const [error, setError] = useState("");

  const [createDialogOpen, setCreateDialogOpen] = useState(false);
  const [deletePoolId, setDeletePoolId] = useState("");
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [poolForm, setPoolForm] = useState({ name: "", warningThresholdPct: 90, devicePath: "" });

  const availableDisks = useMemo(() => disks.filter((disk) => disk.availability === "available"), [disks]);

  async function reloadAll() {
    setError("");
    try {
      const [diskRows, poolRows] = await Promise.all([api.storage.discoverDisks(), api.storage.listPools()]);
      setDisks(diskRows);
      setPools(poolRows);
    } catch (err) {
      setError((err as Error).message || t("messages.apiError"));
    }
  }

  useEffect(() => {
    void reloadAll();
  }, []);

  useEffect(() => {
    if (!createDialogOpen) {
      return;
    }
    setPoolForm((prev) => {
      if (prev.devicePath || availableDisks.length === 0) {
        return prev;
      }
      return { ...prev, devicePath: availableDisks[0].devicePath };
    });
  }, [availableDisks, createDialogOpen]);

  async function createPool(event: FormEvent) {
    event.preventDefault();
    if (!poolForm.name.trim() || !poolForm.devicePath) {
      push(t("messages.requestFailed"), "error");
      return;
    }
    let createdPoolId = "";
    setCreating(true);
    try {
      const poolId = nextAutoPoolId(pools);
      createdPoolId = poolId;
      const created = await api.storage.createPool({
        poolId,
        name: poolForm.name.trim(),
        warningThresholdPct: poolForm.warningThresholdPct,
        actor: "web-console",
      });
      await api.storage.attachDisk(created.poolId, { devicePath: poolForm.devicePath, actor: "web-console" });
      push(t("messages.requestSuccess"), "success");
      setPoolForm({ name: "", warningThresholdPct: 90, devicePath: "" });
      setCreateDialogOpen(false);
      await reloadAll();
    } catch (err) {
      if (createdPoolId) {
        try {
          await api.storage.deletePool(createdPoolId, "web-console");
        } catch {
          // ignore rollback failure
        }
      }
      push((err as Error).message || t("messages.requestFailed"), "error");
    } finally {
      setCreating(false);
    }
  }

  async function deletePool(poolId: string) {
    setDeleting(true);
    try {
      await api.storage.deletePool(poolId, "web-console");
      push(t("messages.requestSuccess"), "success");
      setDeletePoolId("");
      await reloadAll();
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    } finally {
      setDeleting(false);
    }
  }

  function openDeletePool(poolId: string) {
    if (deleting) {
      return;
    }
    setDeletePoolId(poolId);
  }

  return (
    <section>
      <div className="page-header">
        <div className="inline-actions" style={{ justifyContent: "space-between", alignItems: "flex-start" }}>
          <div>
            <h1 className="page-title">{t("storage.title")}</h1>
          </div>
          <button className="btn btn-primary" type="button" onClick={() => setCreateDialogOpen(true)}>
            <Plus size={14} />
            {t("storage.openCreatePool")}
          </button>
        </div>
      </div>

      {error ? <p className="notice notice-error">{error}</p> : null}

      <div className="panel">
        <div className="storage-panel-header">
          <h3>{t("storage.discovery")}</h3>
          <button className="btn" type="button" onClick={() => void reloadAll()}>
            <RefreshCw size={14} />
            {t("storage.discover")}
          </button>
        </div>
        <div className="table-wrap">
          <table className="table table-fixed">
            <colgroup>
              <col style={{ width: "32%" }} />
              <col style={{ width: "18%" }} />
              <col style={{ width: "22%" }} />
              <col style={{ width: "28%" }} />
            </colgroup>
            <thead>
              <tr>
                <th>{t("storage.devicePath")}</th>
                <th>{t("common.capacity")}</th>
                <th>{t("storage.availability")}</th>
                <th>{t("storage.poolBinding")}</th>
              </tr>
            </thead>
            <tbody>
              {disks.map((disk) => (
                <tr key={disk.devicePath}>
                  <td>{disk.devicePath}</td>
                  <td>{formatBytes(disk.sizeBytes)}</td>
                  <td>{diskAvailabilityLabel(disk, t)}</td>
                  <td>{disk.poolId || "-"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      <div className="panel" style={{ marginTop: 12 }}>
        <h3>{t("storage.pools")}</h3>
        <div className="table-wrap">
          <table className="table table-fixed">
            <colgroup>
              <col style={{ width: "14%" }} />
              <col style={{ width: "18%" }} />
              <col style={{ width: "14%" }} />
              <col style={{ width: "15%" }} />
              <col style={{ width: "15%" }} />
              <col style={{ width: "15%" }} />
              <col style={{ width: "9%" }} />
            </colgroup>
            <thead>
              <tr>
                <th>{t("storage.poolId")}</th>
                <th>{t("common.name")}</th>
                <th>{t("common.status")}</th>
                <th>{t("common.capacity")}</th>
                <th>{t("common.used")}</th>
                <th>{t("common.free")}</th>
                <th>{t("common.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {pools.map((pool) => (
                <tr key={pool.poolId}>
                  <td>{pool.poolId}</td>
                  <td>{pool.name}</td>
                  <td>{pool.status}</td>
                  <td>{formatBytes(pool.capacity.totalBytes)}</td>
                  <td>{formatBytes(pool.capacity.usedBytes)}</td>
                  <td>{formatBytes(pool.capacity.freeBytes)}</td>
                  <td>
                    <button
                      className="btn btn-danger"
                      disabled={pool.capacity.usedBytes > 0 || deleting}
                      title={
                        pool.capacity.usedBytes > 0
                          ? t("storage.deleteBlockedUsed")
                          : t("storage.deletePool")
                      }
                      onClick={() => openDeletePool(pool.poolId)}
                    >
                      {pool.capacity.usedBytes > 0 ? null : <Trash2 size={14} />}
                      {pool.capacity.usedBytes > 0 ? t("storage.inUse") : t("common.delete")}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      {createDialogOpen ? (
        <div className="modal-backdrop" role="dialog" aria-modal="true" onClick={() => setCreateDialogOpen(false)}>
          <div className="modal-card" onClick={(event) => event.stopPropagation()}>
            <div className="inline-actions" style={{ justifyContent: "space-between", alignItems: "center", marginBottom: 10 }}>
              <h3 style={{ margin: 0 }}>{t("storage.createPoolDialogTitle")}</h3>
              <button className="btn btn-quiet" type="button" onClick={() => setCreateDialogOpen(false)}>
                {t("common.close")}
              </button>
            </div>
            {availableDisks.length === 0 ? <p className="notice">{t("storage.noAttachableDisk")}</p> : null}
            <form className="form-grid" onSubmit={createPool}>
              <div className="form-row">
                <label>{t("common.name")}</label>
                <input
                  className="input"
                  value={poolForm.name}
                  onChange={(e) => setPoolForm((prev) => ({ ...prev, name: e.target.value }))}
                  required
                />
              </div>
              <div className="form-row">
                <label>{t("storage.warningThreshold")}</label>
                <input
                  className="input"
                  type="number"
                  min={50}
                  max={99}
                  value={poolForm.warningThresholdPct}
                  onChange={(e) =>
                    setPoolForm((prev) => ({ ...prev, warningThresholdPct: Number.parseInt(e.target.value || "90", 10) }))
                  }
                />
              </div>
              <div className="form-row">
                <label>{t("storage.selectDisk")}</label>
                <SelectInput
                  value={poolForm.devicePath}
                  onChange={(value) => setPoolForm((prev) => ({ ...prev, devicePath: value }))}
                  options={[
                    { value: "", label: t("common.noSelection") },
                    ...availableDisks.map((disk) => ({
                      value: disk.devicePath,
                      label: `${disk.devicePath} (${formatBytes(disk.sizeBytes)})`,
                    })),
                  ]}
                  ariaLabel={t("storage.selectDisk")}
                  required
                />
              </div>
              <div className="inline-actions" style={{ gridColumn: "1 / -1" }}>
                <button className="btn btn-primary" type="submit" disabled={creating || availableDisks.length === 0}>
                  {creating ? t("common.loading") : t("common.create")}
                </button>
                <button className="btn btn-quiet" type="button" onClick={() => setCreateDialogOpen(false)}>
                  {t("common.cancel")}
                </button>
              </div>
            </form>
          </div>
        </div>
      ) : null}

      <ConfirmDialog
        open={Boolean(deletePoolId)}
        title={t("storage.deletePoolTitle")}
        message={t("storage.deletePoolMessage", { poolId: deletePoolId })}
        confirmLabel={t("common.delete")}
        danger
        busy={deleting}
        onConfirm={() => void deletePool(deletePoolId)}
        onCancel={() => {
          if (!deleting) {
            setDeletePoolId("");
          }
        }}
      />
    </section>
  );
}
