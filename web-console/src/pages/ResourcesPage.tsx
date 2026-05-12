import { FormEvent, useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Plus } from "lucide-react";
import { api } from "../services/api";
import { useToast } from "../components/Toast";
import { SelectInput } from "../components/SelectInput";
import type { VirtualCartridge, VirtualDrive, VirtualLibrary } from "../services/types";
import {
  DEFAULT_DRIVE_OPTION,
  DEFAULT_LIBRARY_OPTION,
  MAX_LIBRARY_DRIVES,
  availableVendors,
  driveTypeOptionsForLibrary,
  libraryTypeOptionsForVendor,
  nextLibraryId,
} from "./resourceOptions";

const DEFAULT_DRIVE_START_ADDRESS = 256;
const DEFAULT_SLOT_START_ADDRESS = 1024;
const DEFAULT_IE_PORT_COUNT = 4;
const DEFAULT_IE_START_ADDRESS = 768;

function normalizeDriveCount(value: number): number {
  if (!Number.isFinite(value)) {
    return 1;
  }
  return Math.max(1, Math.min(MAX_LIBRARY_DRIVES, Math.floor(value)));
}

export function ResourcesPage() {
  const { t } = useTranslation();
  const { push } = useToast();
  const navigate = useNavigate();

  const [libraries, setLibraries] = useState<VirtualLibrary[]>([]);
  const [drives, setDrives] = useState<VirtualDrive[]>([]);
  const [cartridges, setCartridges] = useState<VirtualCartridge[]>([]);
  const [error, setError] = useState("");

  const [vtlDialogOpen, setVtlDialogOpen] = useState(false);
  const [creating, setCreating] = useState(false);

  const [vtlForm, setVtlForm] = useState({
    name: "",
    vendor: DEFAULT_LIBRARY_OPTION.vendor,
    libraryType: DEFAULT_LIBRARY_OPTION.libraryType,
    driveType: DEFAULT_DRIVE_OPTION.driveType,
    driveCount: 1,
    slotCount: 20,
    iePortCount: DEFAULT_IE_PORT_COUNT,
    compressionEnabled: false,
    dedupEnabled: false,
  });

  const libraryTypeOptions = useMemo(
    () => libraryTypeOptionsForVendor(vtlForm.vendor),
    [vtlForm.vendor]
  );
  const driveTypeOptions = useMemo(
    () => driveTypeOptionsForLibrary(vtlForm.libraryType),
    [vtlForm.libraryType]
  );

  const driveCountMap = useMemo(() => {
    const map = new Map<string, number>();
    for (const drive of drives) {
      map.set(drive.libraryId, (map.get(drive.libraryId) || 0) + 1);
    }
    return map;
  }, [drives]);

  const cartridgeCountMap = useMemo(() => {
    const map = new Map<string, number>();
    for (const cartridge of cartridges) {
      map.set(cartridge.libraryId, (map.get(cartridge.libraryId) || 0) + 1);
    }
    return map;
  }, [cartridges]);

  async function reloadAll() {
    setError("");
    try {
      const [libRows, driveRows, cartRows] = await Promise.all([
        api.resources.listLibraries(),
        api.resources.listDrives(),
        api.resources.listCartridges(),
      ]);
      setLibraries(libRows);
      setDrives(driveRows);
      setCartridges(cartRows);
    } catch (err) {
      setError((err as Error).message || t("messages.apiError"));
    }
  }

  useEffect(() => {
    void reloadAll();
  }, []);

  useEffect(() => {
    if (libraryTypeOptions.length === 0) {
      return;
    }
    if (!libraryTypeOptions.some((item) => item.libraryType === vtlForm.libraryType)) {
      const nextLibraryType = libraryTypeOptions[0].libraryType;
      const nextDrive = driveTypeOptionsForLibrary(nextLibraryType)[0] || DEFAULT_DRIVE_OPTION;
      setVtlForm((prev) => ({
        ...prev,
        libraryType: nextLibraryType,
        driveType: nextDrive.driveType,
      }));
    }
  }, [libraryTypeOptions, vtlForm.libraryType]);

  useEffect(() => {
    if (driveTypeOptions.length === 0) {
      return;
    }
    if (!driveTypeOptions.some((item) => item.driveType === vtlForm.driveType)) {
      setVtlForm((prev) => ({ ...prev, driveType: driveTypeOptions[0].driveType }));
    }
  }, [driveTypeOptions, vtlForm.driveType]);

  async function createVtl(event: FormEvent) {
    event.preventDefault();
    const trimmedName = vtlForm.name.trim();
    if (!trimmedName) {
      push(t("messages.requestFailed"), "error");
      return;
    }

    setCreating(true);
    try {
      const libraryId = nextLibraryId(trimmedName, libraries);
      const driveCount = normalizeDriveCount(vtlForm.driveCount);
      await api.resources.createLibrary({
        libraryId,
        name: trimmedName,
        vendor: vtlForm.vendor,
        libraryType: vtlForm.libraryType,
        driveType: vtlForm.driveType,
        driveCount,
        driveStartAddress: DEFAULT_DRIVE_START_ADDRESS,
        slotCount: vtlForm.slotCount,
        slotStartAddress: DEFAULT_SLOT_START_ADDRESS,
        iePortCount: vtlForm.iePortCount,
        ieStartAddress: DEFAULT_IE_START_ADDRESS,
        compressionEnabled: vtlForm.compressionEnabled,
        dedupEnabled: vtlForm.dedupEnabled,
      });

      for (let idx = 0; idx < driveCount; idx += 1) {
        const driveId = `${libraryId}-drv-${String(idx + 1).padStart(2, "0")}`;
        await api.resources.createDrive({
          driveId,
          libraryId,
          slot: DEFAULT_DRIVE_START_ADDRESS + idx,
        });
      }

      push(t("messages.requestSuccess"), "success");
      setVtlDialogOpen(false);
      setVtlForm({
        name: "",
        vendor: DEFAULT_LIBRARY_OPTION.vendor,
        libraryType: DEFAULT_LIBRARY_OPTION.libraryType,
        driveType: DEFAULT_DRIVE_OPTION.driveType,
        driveCount: 1,
        slotCount: 20,
        iePortCount: DEFAULT_IE_PORT_COUNT,
        compressionEnabled: false,
        dedupEnabled: false,
      });
      await reloadAll();
      navigate(`/resources/${libraryId}/manage`);
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    } finally {
      setCreating(false);
    }
  }

  return (
    <section>
      <div className="page-header">
        <div className="inline-actions" style={{ justifyContent: "space-between", alignItems: "flex-start" }}>
          <div>
            <h1 className="page-title">{t("resources.title")}</h1>
          </div>
          <button className="btn btn-primary" type="button" onClick={() => setVtlDialogOpen(true)}>
            <Plus size={14} />
            {t("resources.createVtl")}
          </button>
        </div>
      </div>

      {error ? <p className="notice notice-error">{error}</p> : null}

      <div className="panel">
        <h3>{t("resources.libraries")}</h3>
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>{t("resources.vtlName")}</th>
                <th>{t("resources.vendor")}</th>
                <th>{t("resources.libraryType")}</th>
                <th>{t("resources.driveType")}</th>
                <th>{t("resources.driveCount")}</th>
                <th>{t("resources.slotCount")}</th>
                <th>{t("resources.cartridges")}</th>
                <th>{t("resources.dataPolicy")}</th>
                <th>{t("resources.iqn")}</th>
              </tr>
            </thead>
            <tbody>
              {libraries.map((library) => (
                <tr
                  className="clickable-table-row"
                  key={library.libraryId}
                  tabIndex={0}
                  title={t("resources.manage")}
                  onClick={() => navigate(`/resources/${library.libraryId}/manage`)}
                  onKeyDown={(event) => {
                    if (event.key === "Enter" || event.key === " ") {
                      event.preventDefault();
                      navigate(`/resources/${library.libraryId}/manage`);
                    }
                  }}
                >
                  <td>{library.name}</td>
                  <td>{library.vendor || "-"}</td>
                  <td>{library.libraryType || "-"}</td>
                  <td>{library.driveType || "-"}</td>
                  <td>{driveCountMap.get(library.libraryId) || library.driveCount || 0}</td>
                  <td>{library.slotCount || 0}</td>
                  <td>{cartridgeCountMap.get(library.libraryId) || 0}</td>
                  <td>
                    <span className="table-chip">{library.compressionEnabled ? t("resources.compression") : t("resources.noCompression")}</span>
                    <span className="table-chip">{library.dedupEnabled ? t("resources.dedup") : t("resources.noDedup")}</span>
                  </td>
                  <td>{library.iqn || "-"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      {vtlDialogOpen ? (
        <div className="modal-backdrop" role="dialog" aria-modal="true" onClick={() => setVtlDialogOpen(false)}>
          <div className="modal-card" onClick={(event) => event.stopPropagation()}>
            <div className="inline-actions" style={{ justifyContent: "space-between", alignItems: "center", marginBottom: 10 }}>
              <h3 style={{ margin: 0 }}>{t("resources.createVtlDialogTitle")}</h3>
              <button className="btn btn-quiet" type="button" onClick={() => setVtlDialogOpen(false)}>
                {t("common.close")}
              </button>
            </div>
            <form className="form-grid" onSubmit={createVtl}>
              <div className="form-row">
                <label>{t("resources.vtlName")}</label>
                <input
                  className="input"
                  value={vtlForm.name}
                  onChange={(event) => setVtlForm((prev) => ({ ...prev, name: event.target.value }))}
                  required
                />
              </div>
              <div className="form-row">
                <label>{t("resources.vendor")}</label>
                <SelectInput
                  value={vtlForm.vendor}
                  onChange={(value) => setVtlForm((prev) => ({ ...prev, vendor: value }))}
                  options={availableVendors().map((vendor) => ({ value: vendor, label: vendor }))}
                  ariaLabel={t("resources.vendor")}
                />
              </div>
              <div className="form-row">
                <label>{t("resources.libraryType")}</label>
                <SelectInput
                  value={vtlForm.libraryType}
                  onChange={(value) => setVtlForm((prev) => ({ ...prev, libraryType: value }))}
                  options={libraryTypeOptions.map((item) => ({ value: item.libraryType, label: item.label }))}
                  ariaLabel={t("resources.libraryType")}
                />
              </div>
              <div className="form-row form-row-wide">
                <label>{t("resources.vDriveType")}</label>
                <SelectInput
                  value={vtlForm.driveType}
                  onChange={(value) => setVtlForm((prev) => ({ ...prev, driveType: value }))}
                  options={driveTypeOptions.map((item) => ({ value: item.driveType, label: item.label }))}
                  ariaLabel={t("resources.vDriveType")}
                />
              </div>
              <div className="form-row">
                <label>{t("resources.driveCount")}</label>
                <input
                  className="input"
                  type="number"
                  min={1}
                  max={MAX_LIBRARY_DRIVES}
                  value={vtlForm.driveCount}
                  onChange={(event) => setVtlForm((prev) => ({ ...prev, driveCount: normalizeDriveCount(Number.parseInt(event.target.value || "1", 10)) }))}
                />
              </div>
              <div className="form-row">
                <label>{t("resources.slotCount")}</label>
                <input
                  className="input"
                  type="number"
                  min={1}
                  value={vtlForm.slotCount}
                  onChange={(event) => setVtlForm((prev) => ({ ...prev, slotCount: Number.parseInt(event.target.value || "20", 10) }))}
                />
              </div>
              <div className="form-row">
                <label>{t("resources.iePortCount")}</label>
                <input
                  className="input"
                  type="number"
                  min={1}
                  max={64}
                  value={vtlForm.iePortCount}
                  onChange={(event) => setVtlForm((prev) => ({ ...prev, iePortCount: Number.parseInt(event.target.value || String(DEFAULT_IE_PORT_COUNT), 10) }))}
                />
              </div>
              <div className="form-row form-row-wide">
                <label>{t("resources.dataPolicy")}</label>
                <div className="policy-toggle-row">
                  <label className="cdb-trace-toggle policy-toggle">
                    <input
                      type="checkbox"
                      checked={vtlForm.compressionEnabled}
                      onChange={(event) => setVtlForm((prev) => ({ ...prev, compressionEnabled: event.target.checked }))}
                    />
                    <span className="switch-track" aria-hidden="true">
                      <span className="switch-thumb" />
                    </span>
                    <span className="switch-label">{t("resources.compression")}</span>
                  </label>
                  <label className="cdb-trace-toggle policy-toggle">
                    <input
                      type="checkbox"
                      checked={vtlForm.dedupEnabled}
                      onChange={(event) => setVtlForm((prev) => ({ ...prev, dedupEnabled: event.target.checked }))}
                    />
                    <span className="switch-track" aria-hidden="true">
                      <span className="switch-thumb" />
                    </span>
                    <span className="switch-label">{t("resources.dedup")}</span>
                  </label>
                </div>
              </div>
              <div className="inline-actions" style={{ gridColumn: "1 / -1" }}>
                <button className="btn btn-primary" type="submit" disabled={creating}>
                  {creating ? t("common.loading") : t("common.create")}
                </button>
                <button className="btn btn-quiet" type="button" onClick={() => setVtlDialogOpen(false)}>
                  {t("common.cancel")}
                </button>
              </div>
            </form>
          </div>
        </div>
      ) : null}

    </section>
  );
}
