import { FormEvent, useEffect, useMemo, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Archive, ChevronLeft, Eraser, HardDrive, Info, Plus, RefreshCw, Trash2, Upload } from "lucide-react";
import { api } from "../services/api";
import { useToast } from "../components/Toast";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { SelectInput } from "../components/SelectInput";
import { formatBytes } from "../utils/format";
import type { ApiError, StoragePoolRuntime, VirtualCartridge, VirtualDrive, VirtualLibrary } from "../services/types";
import { MAX_LIBRARY_DRIVES, nextDriveSuffix, resolveTapeProfileFromDriveType } from "./resourceOptions";

type DeleteTarget =
  | { kind: "library"; id: string }
  | { kind: "drive"; id: string }
  | { kind: "cartridge"; id: string }
  | null;

type EraseTarget = { id: string; mode: "short" | "long" } | null;

type SlotShortageImportTarget = { cartridgeId: string; barcode: string } | null;

type TopologySelection =
  | { kind: "library" }
  | { kind: "drive"; id: string }
  | { kind: "cartridge"; id: string }
  | { kind: "slot"; address: number; index: number }
  | null;

function clampPercent(value: number): number {
  if (!Number.isFinite(value)) {
    return 0;
  }
  return Math.max(0, Math.min(100, value));
}

function cartridgeUsedPercent(cartridge: VirtualCartridge): number {
  if (!cartridge.capacityBytes || cartridge.capacityBytes <= 0) {
    return 0;
  }
  return clampPercent((cartridge.usedBytes / cartridge.capacityBytes) * 100);
}

function formatCapacityPercent(value: number): string {
  return `${Math.round(value)}%`;
}

function normalizeCartridgePrefix(value: string): string {
  return value.toUpperCase().replace(/[^A-Z0-9]/g, "").slice(0, 3);
}

function isLibraryOnline(status: string | undefined): boolean {
  const normalized = (status || "").toLowerCase();
  return normalized === "online" || normalized === "active" || normalized === "ready";
}

function mountedCartridgeId(drive: VirtualDrive | null): string {
  if (!drive) {
    return "";
  }
  return drive.mountedCartridgeId || drive.loadedCartridgeId || "";
}

function driveMountState(drive: VirtualDrive | null): string {
  return drive?.mountState || drive?.status || "empty";
}

type CapacityUnit = "TB" | "GB" | "MB";

const CAPACITY_UNIT_BYTES: Record<CapacityUnit, number> = {
  TB: 1024 ** 4,
  GB: 1024 ** 3,
  MB: 1024 ** 2,
};

function resolveCustomCapacityBytes(rawValue: string, unit: CapacityUnit, fallbackBytes: number): number {
  const parsed = Number.parseFloat(rawValue);
  if (!Number.isFinite(parsed) || parsed <= 0) {
    return fallbackBytes;
  }
  return Math.round(parsed * CAPACITY_UNIT_BYTES[unit]);
}

export function ResourceManagePage() {
  const { t } = useTranslation();
  const { push } = useToast();
  const navigate = useNavigate();
  const { libraryId = "" } = useParams();

  const [libraries, setLibraries] = useState<VirtualLibrary[]>([]);
  const [drives, setDrives] = useState<VirtualDrive[]>([]);
  const [cartridges, setCartridges] = useState<VirtualCartridge[]>([]);
  const [pools, setPools] = useState<StoragePoolRuntime[]>([]);

  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const [createDriveOpen, setCreateDriveOpen] = useState(false);
  const [createCartridgeOpen, setCreateCartridgeOpen] = useState(false);
  const [busyCreateDrive, setBusyCreateDrive] = useState(false);
  const [busyCreateCartridge, setBusyCreateCartridge] = useState(false);
  const [busyResourceAction, setBusyResourceAction] = useState("");
  const [busyDelete, setBusyDelete] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<DeleteTarget>(null);
  const [eraseTarget, setEraseTarget] = useState<EraseTarget>(null);
  const [slotShortageImportTarget, setSlotShortageImportTarget] = useState<SlotShortageImportTarget>(null);
  const [selectedNode, setSelectedNode] = useState<TopologySelection>({ kind: "library" });

  const [driveForm, setDriveForm] = useState({ driveId: "", slot: 1 });
  const [cartridgeAdvancedOpen, setCartridgeAdvancedOpen] = useState(false);
  const [cartridgeForm, setCartridgeForm] = useState({
    poolId: "",
    prefix: "VTA",
    quantity: "1",
    customCapacityValue: "",
    customCapacityUnit: "TB" as CapacityUnit,
    expandSlots: false,
  });
  const [loadTargetDriveId, setLoadTargetDriveId] = useState("");

  const library = useMemo(
    () => libraries.find((item) => item.libraryId === libraryId) || null,
    [libraries, libraryId]
  );

  const libraryDrives = useMemo(
    () => drives.filter((item) => item.libraryId === libraryId).sort((a, b) => a.slot - b.slot),
    [drives, libraryId]
  );
  const canAddDrive = libraryDrives.length < MAX_LIBRARY_DRIVES;

  const libraryCartridges = useMemo(
    () => cartridges.filter((item) => item.libraryId === libraryId).sort((a, b) => a.barcode.localeCompare(b.barcode)),
    [cartridges, libraryId]
  );

  const vaultCartridges = useMemo(
    () => libraryCartridges.filter((item) => item.lifecycleState === "exported"),
    [libraryCartridges]
  );

  const slotCartridges = useMemo(
    () => libraryCartridges.filter((item) => item.lifecycleState !== "exported" && item.lifecycleState !== "mounted"),
    [libraryCartridges]
  );

  const loadableDrives = useMemo(
    () => libraryDrives.filter((drive) => driveMountState(drive) !== "loaded" && !mountedCartridgeId(drive)),
    [libraryDrives]
  );

  const usablePools = useMemo(() => pools.filter((pool) => pool.disks.length > 0), [pools]);
  const poolById = useMemo(() => {
    const map = new Map<string, StoragePoolRuntime>();
    for (const pool of pools) {
      map.set(pool.poolId, pool);
    }
    return map;
  }, [pools]);
  const cartridgeByLabel = useMemo(() => {
    const map = new Map<string, VirtualCartridge>();
    for (const cartridge of libraryCartridges) {
      for (const label of [cartridge.cartridgeId, cartridge.barcode]) {
        if (label) {
          map.set(label.toUpperCase(), cartridge);
        }
      }
    }
    return map;
  }, [libraryCartridges]);
  const tapeProfile = useMemo(
    () => resolveTapeProfileFromDriveType(library?.driveType),
    [library?.driveType]
  );

  const slotStartAddress = library?.slotStartAddress ?? 1;
  const reportedMaxSlotIndex = slotCartridges.reduce((maxIndex, cartridge) => {
    if (cartridge.currentElementAddress == null || cartridge.currentElementAddress < slotStartAddress) {
      return maxIndex;
    }
    return Math.max(maxIndex, cartridge.currentElementAddress - slotStartAddress);
  }, -1);
  const slotCount = Math.max(library?.slotCount || 0, reportedMaxSlotIndex + 1, 1);
  const occupiedSlots = Math.min(slotCartridges.length, slotCount);
  const emptySlots = Math.max(slotCount - occupiedSlots, 0);
  const requestedCartridgeCount = Number.parseInt(cartridgeForm.quantity, 10);
  const cartridgeCreateNeedsSlots = Number.isFinite(requestedCartridgeCount) && requestedCartridgeCount > emptySlots;

  const slotCells = useMemo(() => {
    const cartridgesByIndex = new Map<number, VirtualCartridge>();
    for (const cartridge of slotCartridges) {
      const address = cartridge.currentElementAddress;
      const index = address == null ? -1 : address - slotStartAddress;
      if (index >= 0 && index < slotCount && !cartridgesByIndex.has(index)) {
        cartridgesByIndex.set(index, cartridge);
      }
    }
    return Array.from({ length: slotCount }, (_, index) => {
      return {
        index,
        address: slotStartAddress + index,
        cartridge: cartridgesByIndex.get(index),
      };
    });
  }, [slotCount, slotCartridges, slotStartAddress]);

  const selectedDrive = useMemo(() => {
    if (!selectedNode || selectedNode.kind !== "drive") {
      return null;
    }
    return libraryDrives.find((drive) => drive.driveId === selectedNode.id) || null;
  }, [libraryDrives, selectedNode]);

  const selectedCartridge = useMemo(() => {
    if (!selectedNode || selectedNode.kind !== "cartridge") {
      return null;
    }
    return libraryCartridges.find((cartridge) => cartridge.cartridgeId === selectedNode.id) || null;
  }, [libraryCartridges, selectedNode]);

  const selectedCartridgeInVault = selectedCartridge?.lifecycleState === "exported";
  const selectedCartridgeCanLoad = selectedCartridge?.lifecycleState === "available" && Boolean(loadTargetDriveId);
  const selectedCartridgeCanExport = selectedCartridge?.lifecycleState === "available";
  const selectedCartridgeCanErase = Boolean(selectedCartridge) && selectedCartridge?.lifecycleState !== "mounted";

  useEffect(() => {
    if (loadableDrives.length === 0) {
      setLoadTargetDriveId("");
      return;
    }
    setLoadTargetDriveId((prev) =>
      loadableDrives.some((drive) => drive.driveId === prev) ? prev : loadableDrives[0].driveId
    );
  }, [loadableDrives]);

  useEffect(() => {
    if (!createDriveOpen || !library) {
      return;
    }
    if (!canAddDrive) {
      setCreateDriveOpen(false);
      return;
    }
    const driveSuffix = String(nextDriveSuffix(drives, library.libraryId)).padStart(2, "0");
    const defaultSlot = library.driveStartAddress && library.driveStartAddress > 0
      ? library.driveStartAddress + libraryDrives.length
      : libraryDrives.length + 1;
    setDriveForm({
      driveId: `${library.libraryId}-drv-${driveSuffix}`,
      slot: defaultSlot,
    });
  }, [canAddDrive, createDriveOpen, library, drives, libraryDrives.length]);

  useEffect(() => {
    if (!createCartridgeOpen) {
      return;
    }
    setCartridgeForm((prev) => ({
      ...prev,
      poolId: prev.poolId || usablePools[0]?.poolId || "",
      prefix: normalizeCartridgePrefix(prev.prefix || "VTA") || "VTA",
      quantity: prev.quantity.trim() === "" ? "1" : prev.quantity,
      customCapacityValue: prev.customCapacityValue,
      customCapacityUnit: prev.customCapacityUnit,
      expandSlots: cartridgeCreateNeedsSlots ? prev.expandSlots : false,
    }));
  }, [cartridgeCreateNeedsSlots, createCartridgeOpen, usablePools]);

  useEffect(() => {
    if (!createDriveOpen && !createCartridgeOpen) {
      return;
    }
    function handleKeyDown(event: KeyboardEvent) {
      if (event.key !== "Escape") {
        return;
      }
      setCreateDriveOpen(false);
      setCreateCartridgeOpen(false);
    }
    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, [createCartridgeOpen, createDriveOpen]);

  async function reloadAll() {
    setError("");
    setLoading(true);
    try {
      const [libRows, driveRows, cartRows, poolRows] = await Promise.all([
        api.resources.listLibraries(),
        api.resources.listDrives(),
        api.resources.listCartridges(),
        api.storage.listPools(),
      ]);
      setLibraries(libRows);
      setDrives(driveRows);
      setCartridges(cartRows);
      setPools(poolRows);
    } catch (err) {
      setError((err as Error).message || t("messages.apiError"));
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    void reloadAll();
  }, [libraryId]);

  async function createDrive(event: FormEvent) {
    event.preventDefault();
    if (!library || !driveForm.driveId.trim() || !canAddDrive) {
      push(t("messages.requestFailed"), "error");
      return;
    }
    setBusyCreateDrive(true);
    try {
      await api.resources.createDrive({
        driveId: driveForm.driveId.trim(),
        libraryId: library.libraryId,
        slot: driveForm.slot,
      });
      push(t("messages.requestSuccess"), "success");
      setCreateDriveOpen(false);
      await reloadAll();
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    } finally {
      setBusyCreateDrive(false);
    }
  }

  async function createCartridge(event: FormEvent) {
    event.preventDefault();
    const quantity = Number.parseInt(cartridgeForm.quantity, 10);
    if (!library || !cartridgeForm.poolId || !Number.isFinite(quantity) || quantity < 1 || quantity > 200) {
      push(!cartridgeForm.poolId ? t("resources.poolRequired") : t("resources.cartridgeCountHint"), "error");
      return;
    }
    if (!normalizeCartridgePrefix(cartridgeForm.prefix)) {
      push(t("resources.cartridgePrefixHint"), "error");
      return;
    }
    if (quantity > emptySlots && !cartridgeForm.expandSlots) {
      push(t("resources.addSlotRequired"), "error");
      return;
    }
    setBusyCreateCartridge(true);
    try {
      const prefix = normalizeCartridgePrefix(cartridgeForm.prefix);
      const capacityBytes = cartridgeAdvancedOpen
        ? resolveCustomCapacityBytes(
            cartridgeForm.customCapacityValue,
            cartridgeForm.customCapacityUnit,
            tapeProfile.capacityBytes
          )
        : tapeProfile.capacityBytes;
      for (let idx = 0; idx < quantity; idx += 1) {
        await api.resources.createCartridge({
          barcodePrefix: prefix,
          poolId: cartridgeForm.poolId,
          libraryId: library.libraryId,
          capacityBytes,
          ltoGeneration: tapeProfile.ltoGeneration,
          mediaType: `LTO${tapeProfile.ltoGeneration}`,
          expandSlots: cartridgeForm.expandSlots,
        });
      }
      push(t("messages.requestSuccess"), "success");
      setCreateCartridgeOpen(false);
      setCartridgeAdvancedOpen(false);
      setCartridgeForm({
        poolId: "",
        prefix: "VTA",
        quantity: "1",
        customCapacityValue: "",
        customCapacityUnit: "TB",
        expandSlots: false,
      });
      await reloadAll();
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    } finally {
      setBusyCreateCartridge(false);
    }
  }

  async function addSlot() {
    if (!library) {
      return;
    }
    setBusyResourceAction("add-slot");
    setError("");
    try {
      await api.resources.addLibrarySlots(library.libraryId, { count: 1, actor: "web-console" });
      push(t("messages.requestSuccess"), "success");
      await reloadAll();
    } catch (err) {
      const message = actionErrorMessage(err);
      setError(message);
      push(message, "error");
    } finally {
      setBusyResourceAction("");
    }
  }

  function actionErrorMessage(err: unknown): string {
    const apiErr = err as Partial<ApiError>;
    if (apiErr?.status === 404 || apiErr?.status === 405) {
      return t("resources.actionApiUnavailable");
    }
    return (apiErr?.message as string) || t("messages.requestFailed");
  }

  async function loadCartridgeToDrive(cartridge: VirtualCartridge, driveId: string) {
    if (!driveId) {
      push(t("resources.noEmptyDrive"), "error");
      return;
    }
    setBusyResourceAction(`load:${cartridge.cartridgeId}`);
    setError("");
    try {
      await api.resources.loadCartridge(driveId, {
        cartridgeId: cartridge.cartridgeId,
        actor: "web-console",
      });
      push(t("messages.requestSuccess"), "success");
      setSelectedNode({ kind: "drive", id: driveId });
      await reloadAll();
    } catch (err) {
      const message = actionErrorMessage(err);
      setError(message);
      push(message, "error");
    } finally {
      setBusyResourceAction("");
    }
  }

  async function unloadDrive(drive: VirtualDrive) {
    setBusyResourceAction(`unload:${drive.driveId}`);
    setError("");
    try {
      await api.resources.unloadDrive(drive.driveId);
      push(t("messages.requestSuccess"), "success");
      setSelectedNode({ kind: "drive", id: drive.driveId });
      await reloadAll();
    } catch (err) {
      const message = actionErrorMessage(err);
      setError(message);
      push(message, "error");
    } finally {
      setBusyResourceAction("");
    }
  }

  async function exportCartridge(cartridge: VirtualCartridge) {
    setBusyResourceAction(`export:${cartridge.cartridgeId}`);
    setError("");
    try {
      await api.resources.exportCartridge(cartridge.cartridgeId);
      push(t("messages.requestSuccess"), "success");
      setSelectedNode({ kind: "cartridge", id: cartridge.cartridgeId });
      await reloadAll();
    } catch (err) {
      const message = actionErrorMessage(err);
      setError(message);
      push(message, "error");
    } finally {
      setBusyResourceAction("");
    }
  }

  async function importCartridge(cartridge: VirtualCartridge) {
    setBusyResourceAction(`import:${cartridge.cartridgeId}`);
    setError("");
    try {
      await api.resources.importCartridge(cartridge.cartridgeId);
      push(t("messages.requestSuccess"), "success");
      setSelectedNode({ kind: "cartridge", id: cartridge.cartridgeId });
      await reloadAll();
    } catch (err) {
      const apiErr = err as Partial<ApiError>;
      if (apiErr?.status === 409) {
        setSlotShortageImportTarget({ cartridgeId: cartridge.cartridgeId, barcode: cartridge.barcode });
        return;
      }
      const message = actionErrorMessage(err);
      setError(message);
      push(message, "error");
    } finally {
      setBusyResourceAction("");
    }
  }

  async function addSlotAndImportCartridge() {
    if (!library || !slotShortageImportTarget) {
      return;
    }
    const target = slotShortageImportTarget;
    setBusyResourceAction(`add-slot-import:${target.cartridgeId}`);
    setError("");
    try {
      await api.resources.addLibrarySlots(library.libraryId, { count: 1, actor: "web-console" });
      await api.resources.importCartridge(target.cartridgeId);
      push(t("messages.requestSuccess"), "success");
      setSelectedNode({ kind: "cartridge", id: target.cartridgeId });
      setSlotShortageImportTarget(null);
      await reloadAll();
    } catch (err) {
      const message = actionErrorMessage(err);
      setError(message);
      push(message, "error");
    } finally {
      setBusyResourceAction("");
    }
  }

  async function confirmErase() {
    if (!eraseTarget) {
      return;
    }
    const cartridge = libraryCartridges.find((item) => item.cartridgeId === eraseTarget.id);
    if (!cartridge) {
      setEraseTarget(null);
      return;
    }
    const mode = eraseTarget.mode;
    setBusyResourceAction(`${mode}-erase-${cartridge.cartridgeId}`);
    setError("");
    try {
      await api.resources.eraseCartridge(cartridge.cartridgeId, { mode, actor: "web-console" });
      push(t("messages.requestSuccess"), "success");
      setSelectedNode({ kind: "cartridge", id: cartridge.cartridgeId });
      await reloadAll();
    } catch (err) {
      const message = actionErrorMessage(err);
      setError(message);
      push(message, "error");
    } finally {
      setBusyResourceAction("");
      setEraseTarget(null);
    }
  }

  async function confirmDelete() {
    if (!deleteTarget || !library) {
      return;
    }
    setBusyDelete(true);
    try {
      if (deleteTarget.kind === "library") {
        await api.resources.deleteLibrary(deleteTarget.id);
        push(t("messages.requestSuccess"), "success");
        navigate("/resources");
        return;
      }
      if (deleteTarget.kind === "drive") {
        await api.resources.deleteDrive(deleteTarget.id);
      }
      if (deleteTarget.kind === "cartridge") {
        await api.resources.deleteCartridge(deleteTarget.id);
      }
      push(t("messages.requestSuccess"), "success");
      setDeleteTarget(null);
      setSelectedNode({ kind: "library" });
      await reloadAll();
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    } finally {
      setBusyDelete(false);
    }
  }

  const deleteDialogTitle = useMemo(() => {
    if (!deleteTarget) {
      return "";
    }
    if (deleteTarget.kind === "library") {
      return t("resources.deleteVtlTitle");
    }
    if (deleteTarget.kind === "drive") {
      return t("resources.deleteDriveTitle");
    }
    return t("resources.destroyCartridgeTitle");
  }, [deleteTarget, t]);

  const deleteDialogMessage = useMemo(() => {
    if (!deleteTarget) {
      return "";
    }
      if (deleteTarget.kind === "library" && library) {
      return t("resources.deleteVtlMessage", {
        name: library.name,
        drives: libraryDrives.length,
        cartridges: libraryCartridges.length,
      });
    }
    if (deleteTarget.kind === "drive") {
      return t("resources.deleteDriveMessage", { driveId: deleteTarget.id });
    }
    return t("resources.destroyCartridgeMessage", { cartridgeId: deleteTarget.id });
  }, [deleteTarget, library, libraryCartridges.length, libraryDrives.length, t]);

  if (loading) {
    return <p className="notice">{t("common.loading")}</p>;
  }

  if (!library) {
    return (
      <section>
        <div className="page-header">
          <h1 className="page-title">{t("resources.manageTitle")}</h1>
        </div>
        <p className="notice">{t("resources.vtlNotFound")}</p>
        <button className="btn" type="button" onClick={() => navigate("/resources")}> 
          <ChevronLeft size={14} />
          {t("resources.backToResources")}
        </button>
      </section>
    );
  }

  const libraryOnline = isLibraryOnline(library.status);

  function cartridgeLifecycleLabel(cartridge: VirtualCartridge): string {
    const state = cartridge.lifecycleState || "";
    const key = `resources.lifecycleStates.${state}`;
    const translated = t(key);
    return translated === key ? state || "-" : translated;
  }

  function driveStateLabel(drive: VirtualDrive): string {
    const state = driveMountState(drive);
    const key = `resources.driveStates.${state}`;
    const translated = t(key);
    return translated === key ? state || "-" : translated;
  }

  return (
    <section>
      {error ? <p className="notice notice-error">{error}</p> : null}

      <div className="vtl-manage-shell">
        <div className="vtl-manage-header">
          <div className="inline-actions" style={{ alignItems: "center" }}>
            <button className="btn btn-quiet" type="button" onClick={() => navigate("/resources")}> 
              <ChevronLeft size={14} />
              {t("resources.backToResources")}
            </button>
            <span
              className={`status-badge status-dot ${libraryOnline ? "status-ok" : "status-error"}`}
              aria-label={libraryOnline ? t("resources.online") : t("resources.offline")}
              title={libraryOnline ? t("resources.online") : t("resources.offline")}
            />
          </div>

          <div className="vtl-manage-title-row">
            <div>
              <h1 className="vtl-manage-title">{library.name}</h1>
              <div className="vtl-manage-meta">
                <span>ID: {library.libraryId}</span>
                <span>{library.iqn || "-"}</span>
                <span>{library.vendor || "-"} {library.libraryType || "-"}</span>
                <span>{library.driveType || "-"}</span>
              </div>
            </div>
            <div className="inline-actions">
              <button className="btn btn-quiet" type="button" onClick={() => void reloadAll()}>
                <RefreshCw size={14} />
                {t("common.refresh")}
              </button>
              {canAddDrive ? (
                <button className="btn btn-primary" type="button" onClick={() => setCreateDriveOpen(true)}>
                  <Plus size={14} />
                  {t("resources.addDrive")}
                </button>
              ) : null}
              <button className="btn btn-primary" type="button" onClick={() => setCreateCartridgeOpen(true)}>
                <Plus size={14} />
                {t("resources.addCartridge")}
              </button>
            </div>
          </div>
        </div>

        <div className="vtl-manage-grid">
          <div className="vtl-topology-card">
            <div className="topology-toolbar">
              <div>
                <h3>{t("resources.rackTopology")}</h3>
              </div>
              <div className="legend-row">
                <span><i className="legend-swatch legend-swatch-slot" />{t("resources.emptySlot")}</span>
                <span><i className="legend-swatch legend-swatch-tape" />{t("resources.loadedTape")}</span>
                <span><i className="legend-swatch legend-swatch-drive" />{t("resources.activeDrive")}</span>
              </div>
            </div>

            <div className="topology-console">
              <aside className="topology-rail">
                <button
                  className={`topology-library-node ${selectedNode?.kind === "library" ? "topology-node-active" : ""}`}
                  type="button"
                  onClick={() => setSelectedNode({ kind: "library" })}
                >
                  <Archive size={18} />
                  <span>{t("resources.librarySummary")}</span>
                  <strong>{library.name}</strong>
                </button>
                <div className="topology-stat-grid">
                  <div><span>{t("resources.slotCount")}</span><strong>{slotCount}</strong></div>
                  <div><span>{t("resources.occupiedSlots")}</span><strong>{occupiedSlots}</strong></div>
                  <div><span>{t("resources.emptySlots")}</span><strong>{emptySlots}</strong></div>
                  <div><span>{t("resources.driveCount")}</span><strong>{libraryDrives.length}</strong></div>
                </div>
                <div className="topology-usage-bar">
                  <span style={{ width: `${slotCount > 0 ? Math.round((occupiedSlots / slotCount) * 100) : 0}%` }} />
                </div>
                <div className="quick-actions">
                  <button className="btn btn-quiet" type="button" onClick={() => void reloadAll()}>
                    <RefreshCw size={14} />
                    {t("resources.rescan")}
                  </button>
                  <button className="btn btn-quiet" type="button" onClick={() => navigate("/targets")}>
                    <HardDrive size={14} />
                    {t("resources.openTargets")}
                  </button>
                  <button
                    className="btn btn-danger"
                    type="button"
                    onClick={() => setDeleteTarget({ kind: "library", id: library.libraryId })}
                  >
                    <Trash2 size={14} />
                    {t("resources.deleteVtl")}
                  </button>
                </div>
              </aside>

              <div className="topology-map">
                <div className="topology-lane-label">
                  <HardDrive size={14} />
                  <span>{t("resources.driveBay")}</span>
                </div>
                <div className="topology-drive-row">
                  {libraryDrives.length === 0 ? <p className="notice">{t("resources.noDrives")}</p> : null}
                  {libraryDrives.map((drive) => {
                    const loadedId = mountedCartridgeId(drive);
                    const loadedCartridge = loadedId ? cartridgeByLabel.get(loadedId.toUpperCase()) : null;
                    const loadedPercent = loadedCartridge ? cartridgeUsedPercent(loadedCartridge) : 0;
                    return (
                      <button
                        className={`topology-drive-node ${loadedId ? "topology-drive-node-loaded" : ""} ${selectedNode?.kind === "drive" && selectedNode.id === drive.driveId ? "topology-node-active" : ""}`}
                        key={drive.driveId}
                        type="button"
                        onClick={() => setSelectedNode({ kind: "drive", id: drive.driveId })}
                      >
                        <span className="drive-node-header">
                          <span className={`drive-status-light ${driveMountState(drive) === "loaded" ? "ready" : ""}`} />
                          <strong>{drive.driveId}</strong>
                        </span>
                        {loadedId ? (
                          <span className="tape-cartridge drive-loaded-tape">
                            <span className="tape-spools" aria-hidden="true">
                              <span />
                              <span />
                            </span>
                            <span className="tape-label">
                              <strong>{loadedId}</strong>
                              <em>{formatCapacityPercent(loadedPercent)}</em>
                            </span>
                            <span className="tape-capacity" aria-label={loadedCartridge ? `${formatBytes(loadedCartridge.usedBytes || 0)} / ${formatBytes(loadedCartridge.capacityBytes)}` : loadedId}>
                              <span style={{ width: `${loadedPercent}%` }} />
                              <em style={{ width: `${100 - loadedPercent}%` }} />
                            </span>
                          </span>
                        ) : (
                          <small>{t("resources.driveEmpty")}</small>
                        )}
                      </button>
                    );
                  })}
                  {canAddDrive ? (
                    <button className="topology-add-node" type="button" onClick={() => setCreateDriveOpen(true)}>
                      <Plus size={16} />
                      <span>{t("resources.addDrive")}</span>
                    </button>
                  ) : null}
                </div>

                <div className="topology-lane-label">
                  <Archive size={14} />
                  <span>{t("resources.mediaSlots")}</span>
                </div>
                <div className="topology-section-scroll topology-slots-scroll">
                  <div className="slots-grid topology-slots" role="list" aria-label={t("resources.slotMap")}>
                    {slotCells.map((cell) => {
                      const selected =
                        cell.cartridge
                          ? selectedNode?.kind === "cartridge" && selectedNode.id === cell.cartridge.cartridgeId
                          : selectedNode?.kind === "slot" && selectedNode.address === cell.address;
                      const usedPercent = cell.cartridge ? cartridgeUsedPercent(cell.cartridge) : 0;
                      const freePercent = 100 - usedPercent;
                      return (
                        <button
                          role="listitem"
                          key={`${cell.address}-${cell.index}`}
                          className={`${cell.cartridge ? "slot-cell slot-cell-used tape-slot" : "slot-cell"} ${selected ? "topology-node-active" : ""}`}
                          title={cell.cartridge ? `${cell.cartridge.barcode} (${cell.cartridge.cartridgeId})` : `${t("resources.slot")}-${cell.address}`}
                          data-slot-address={cell.address}
                          type="button"
                          onClick={() =>
                            setSelectedNode(
                              cell.cartridge
                                ? { kind: "cartridge", id: cell.cartridge.cartridgeId }
                                : { kind: "slot", address: cell.address, index: cell.index }
                            )
                          }
                        >
                          <small className="slot-address">{cell.address}</small>
                          {cell.cartridge ? (
                            <span className="tape-cartridge">
                              <span className="tape-spools" aria-hidden="true">
                                <span />
                                <span />
                              </span>
                              <span className="tape-label">
                                <strong>{cell.cartridge.barcode}</strong>
                                <em>{formatCapacityPercent(usedPercent)}</em>
                              </span>
                              <span className="tape-capacity" aria-label={`${formatBytes(cell.cartridge.usedBytes || 0)} / ${formatBytes(cell.cartridge.capacityBytes)}`}>
                                <span style={{ width: `${usedPercent}%` }} />
                                <em style={{ width: `${freePercent}%` }} />
                              </span>
                            </span>
                          ) : (
                            <strong>-</strong>
                          )}
                        </button>
                      );
                    })}
                    <button
                      className="topology-add-slot"
                      type="button"
                      disabled={busyResourceAction === "add-slot"}
                      onClick={() => void addSlot()}
                    >
                      <Plus size={16} />
                      <span>{busyResourceAction === "add-slot" ? t("common.loading") : t("resources.addSlot")}</span>
                    </button>
                  </div>
                </div>

                <div className="topology-lane-label">
                  <Archive size={14} />
                  <span>{t("resources.vaultLibrary")}</span>
                </div>
                <div className="topology-section-scroll topology-vault-scroll">
                  <div className="vault-row" role="list" aria-label={t("resources.vaultLibrary")}>
                    {vaultCartridges.length === 0 ? (
                      <div className="vault-empty" role="listitem">
                        <span className="vault-empty-icon" aria-hidden="true">
                          <Archive size={15} />
                        </span>
                        <strong>{t("resources.vaultEmpty")}</strong>
                      </div>
                    ) : null}
                    {vaultCartridges.map((cartridge) => {
                      const usedPercent = cartridgeUsedPercent(cartridge);
                      return (
                        <button
                          className={`vault-tape ${selectedNode?.kind === "cartridge" && selectedNode.id === cartridge.cartridgeId ? "topology-node-active" : ""}`}
                          key={cartridge.cartridgeId}
                          type="button"
                          onClick={() => setSelectedNode({ kind: "cartridge", id: cartridge.cartridgeId })}
                        >
                          <span className="tape-cartridge tape-cartridge-vault">
                            <span className="tape-spools" aria-hidden="true">
                              <span />
                              <span />
                            </span>
                            <span className="tape-label">
                              <strong>{cartridge.barcode}</strong>
                              <em>{formatCapacityPercent(usedPercent)}</em>
                            </span>
                            <span className="vault-tape-capacity-text">{formatBytes(cartridge.usedBytes || 0)} / {formatBytes(cartridge.capacityBytes)}</span>
                            <span className="tape-capacity" aria-label={`${formatBytes(cartridge.usedBytes || 0)} / ${formatBytes(cartridge.capacityBytes)}`}>
                              <span style={{ width: `${usedPercent}%` }} />
                              <em style={{ width: `${100 - usedPercent}%` }} />
                            </span>
                          </span>
                        </button>
                      );
                    })}
                  </div>
                </div>
              </div>

              <aside className="topology-inspector">
                <div className="topology-inspector-title">
                  <Info size={16} />
                  <h3>{t("resources.selectionDetails")}</h3>
                </div>

                {selectedNode?.kind === "library" || !selectedNode ? (
                  <div className="inspector-body">
                    <strong className="inspector-title-value">{library.name}</strong>
                    <div className="inspector-field">
                      <span>{t("common.status")}</span>
                      <strong>{libraryOnline ? t("resources.online") : t("resources.offline")}</strong>
                    </div>
                    <div className="inspector-field">
                      <span>{t("resources.slotCount")}</span>
                      <strong>{slotCount}</strong>
                    </div>
                    <div className="inspector-field">
                      <span>{t("resources.driveCount")}</span>
                      <strong>{libraryDrives.length}</strong>
                    </div>
                  </div>
                ) : null}

                {selectedDrive ? (
                  <div className="inspector-body">
                    <strong className="inspector-title-value">{selectedDrive.driveId}</strong>
                    <div className="inspector-field">
                      <span>{t("common.status")}</span>
                      <strong>{driveStateLabel(selectedDrive)}</strong>
                    </div>
                    <div className="inspector-field">
                      <span>{t("resources.loaded")}</span>
                      <strong>{mountedCartridgeId(selectedDrive) || "-"}</strong>
                    </div>
                    <div className="inspector-field">
                      <span>{t("resources.driveIqn")}</span>
                      <strong>{selectedDrive.iqn || "-"}</strong>
                    </div>
                    {mountedCartridgeId(selectedDrive) ? (
                      <button
                        className="btn btn-quiet"
                        type="button"
                        disabled={Boolean(busyResourceAction)}
                        onClick={() => void unloadDrive(selectedDrive)}
                      >
                        <Archive size={14} />
                        {t("resources.unloadFromDrive")}
                      </button>
                    ) : null}
                    <button className="btn btn-danger" type="button" onClick={() => setDeleteTarget({ kind: "drive", id: selectedDrive.driveId })}>
                      <Trash2 size={14} />
                      {t("common.delete")}
                    </button>
                  </div>
                ) : null}

                {selectedCartridge ? (
                  <div className="inspector-body">
                    <strong className="inspector-title-value">{selectedCartridge.barcode}</strong>
                    <div className="inspector-field">
                      <span>{t("resources.cartridgeId")}</span>
                      <strong>{selectedCartridge.cartridgeId}</strong>
                    </div>
                    <div className="inspector-field">
                      <span>{t("storage.poolId")}</span>
                      <strong>{selectedCartridge.poolId}</strong>
                    </div>
                    <div className="inspector-field">
                      <span>{t("resources.poolName")}</span>
                      <strong>{poolById.get(selectedCartridge.poolId)?.name || "-"}</strong>
                    </div>
                    <div className="inspector-field">
                      <span>{t("resources.cartridgeLocation")}</span>
                      <strong>{cartridgeLifecycleLabel(selectedCartridge)}</strong>
                    </div>
                    <div className="inspector-field">
                      <span>{t("common.capacity")}</span>
                      <strong>{formatBytes(selectedCartridge.capacityBytes)}</strong>
                    </div>
                    <div className="inspector-field">
                      <span>{t("common.used")}</span>
                      <strong>{formatBytes(selectedCartridge.usedBytes || 0)}</strong>
                    </div>
                    <div className="inspector-capacity">
                      <span style={{ width: `${cartridgeUsedPercent(selectedCartridge)}%` }} />
                    </div>
                    {!selectedCartridgeInVault ? (
                      <div className="form-row inspector-load-target-row">
                        <label>{t("resources.targetDrive")}</label>
                        <SelectInput
                          value={loadTargetDriveId}
                          onChange={setLoadTargetDriveId}
                          options={[
                            { value: "", label: t("common.noSelection") },
                            ...loadableDrives.map((drive) => ({ value: drive.driveId, label: drive.driveId })),
                          ]}
                          ariaLabel={t("resources.targetDrive")}
                          disabled={loadableDrives.length === 0 || selectedCartridge.lifecycleState !== "available" || Boolean(busyResourceAction)}
                        />
                        {loadableDrives.length === 0 ? <small>{t("resources.noEmptyDrive")}</small> : null}
                      </div>
                    ) : null}
                    <div className="inspector-action-grid">
                      <button
                        className="btn btn-quiet"
                        type="button"
                        disabled={!selectedCartridgeCanLoad || Boolean(busyResourceAction)}
                        title={selectedCartridgeCanLoad ? t("resources.loadToDrive") : t("resources.noEmptyDrive")}
                        onClick={() => void loadCartridgeToDrive(selectedCartridge, loadTargetDriveId)}
                      >
                        <HardDrive size={14} />
                        {t("resources.loadToDrive")}
                      </button>
                      {selectedCartridgeInVault ? (
                        <button
                          className="btn btn-quiet"
                          type="button"
                          disabled={Boolean(busyResourceAction)}
                          onClick={() => void importCartridge(selectedCartridge)}
                        >
                          <Upload size={14} />
                          {t("resources.importFromVault")}
                        </button>
                      ) : (
                        <button
                          className="btn btn-quiet"
                          type="button"
                          disabled={!selectedCartridgeCanExport || Boolean(busyResourceAction)}
                          onClick={() => void exportCartridge(selectedCartridge)}
                        >
                          <Archive size={14} />
                          {t("resources.exportToVault")}
                        </button>
                      )}
                      <button
                        className="btn btn-quiet"
                        type="button"
                        disabled={!selectedCartridgeCanErase || Boolean(busyResourceAction)}
                        onClick={() => setEraseTarget({ id: selectedCartridge.cartridgeId, mode: "short" })}
                      >
                        <Eraser size={14} />
                        {t("resources.shortErase")}
                      </button>
                      <button
                        className="btn btn-quiet"
                        type="button"
                        disabled={!selectedCartridgeCanErase || Boolean(busyResourceAction)}
                        onClick={() => setEraseTarget({ id: selectedCartridge.cartridgeId, mode: "long" })}
                      >
                        <Eraser size={14} />
                        {t("resources.longErase")}
                      </button>
                    </div>
                    <button className="btn btn-danger" type="button" onClick={() => setDeleteTarget({ kind: "cartridge", id: selectedCartridge.cartridgeId })}>
                      <Trash2 size={14} />
                      {t("resources.destroyCartridge")}
                    </button>
                  </div>
                ) : null}

                {selectedNode?.kind === "slot" ? (
                  <div className="inspector-body">
                    <strong className="inspector-title-value">{t("resources.emptySlot")}</strong>
                    <div className="inspector-field">
                      <span>{t("resources.slot")}</span>
                      <strong>{selectedNode.address}</strong>
                    </div>
                    <div className="inspector-field">
                      <span>{t("resources.slotIndex")}</span>
                      <strong>{selectedNode.index + 1}</strong>
                    </div>
                    <button className="btn btn-primary" type="button" onClick={() => setCreateCartridgeOpen(true)}>
                      <Plus size={14} />
                      {t("resources.addCartridge")}
                    </button>
                  </div>
                ) : null}

              </aside>
            </div>
          </div>
        </div>
      </div>

      {createDriveOpen ? (
        <div className="modal-backdrop" role="dialog" aria-modal="true" onClick={() => setCreateDriveOpen(false)}>
          <div className="modal-card" onClick={(event) => event.stopPropagation()}>
            <div className="inline-actions" style={{ justifyContent: "space-between", alignItems: "center", marginBottom: 10 }}>
              <h3 style={{ margin: 0 }}>{t("resources.addDrive")}</h3>
            </div>
            <form className="form-grid" onSubmit={createDrive}>
              <div className="form-row">
                <label>{t("resources.driveId")}</label>
                <input
                  className="input"
                  value={driveForm.driveId}
                  onChange={(event) => setDriveForm((prev) => ({ ...prev, driveId: event.target.value }))}
                  required
                />
              </div>
              <div className="inline-actions" style={{ gridColumn: "1 / -1" }}>
                <button className="btn btn-primary" type="submit" disabled={busyCreateDrive}>{busyCreateDrive ? t("common.loading") : t("common.create")}</button>
                <button className="btn btn-quiet" type="button" onClick={() => setCreateDriveOpen(false)}>{t("common.cancel")}</button>
              </div>
            </form>
          </div>
        </div>
      ) : null}

      {createCartridgeOpen ? (
        <div className="modal-backdrop" role="dialog" aria-modal="true" onClick={() => setCreateCartridgeOpen(false)}>
          <div className="modal-card" onClick={(event) => event.stopPropagation()}>
            <div className="inline-actions" style={{ justifyContent: "space-between", alignItems: "center", marginBottom: 10 }}>
              <h3 style={{ margin: 0 }}>{t("resources.addCartridge")}</h3>
            </div>
            <form className="form-grid" onSubmit={createCartridge}>
              <div className="form-row form-row-wide">
                <label>{t("resources.poolName")}</label>
                <SelectInput
                  value={cartridgeForm.poolId}
                  onChange={(value) => setCartridgeForm((prev) => ({ ...prev, poolId: value }))}
                  options={[
                    { value: "", label: t("common.noSelection") },
                    ...usablePools.map((pool) => ({ value: pool.poolId, label: `${pool.name} (${pool.poolId})` })),
                  ]}
                  ariaLabel={t("resources.poolName")}
                  required
                />
              </div>
              <div className="form-row">
                <label>{t("resources.cartridgePrefix")}</label>
                <input
                  className="input"
                  value={cartridgeForm.prefix}
                  maxLength={3}
                  pattern="[A-Za-z0-9]{1,3}"
                  title={t("resources.cartridgePrefixHint")}
                  onChange={(event) =>
                    setCartridgeForm((prev) => ({ ...prev, prefix: normalizeCartridgePrefix(event.target.value) }))
                  }
                  required
                />
              </div>
              <div className="form-row">
                <label htmlFor="cartridge-count-input">{t("resources.cartridgeCount")}</label>
                <input
                  id="cartridge-count-input"
                  className="input"
                  type="number"
                  min={1}
                  max={200}
                  value={cartridgeForm.quantity}
                  onChange={(event) => setCartridgeForm((prev) => ({ ...prev, quantity: event.target.value }))}
                  onBlur={() =>
                    setCartridgeForm((prev) => {
                      const parsed = Number.parseInt(prev.quantity, 10);
                      const next = Number.isFinite(parsed) ? Math.min(200, Math.max(1, parsed)) : 1;
                      return { ...prev, quantity: String(next) };
                    })
                  }
                  required
                />
              </div>
              {cartridgeCreateNeedsSlots ? (
                <div className="modal-notice modal-notice-stack" style={{ gridColumn: "1 / -1" }}>
                  <span>{t("resources.addSlotRequiredDescription", { count: Math.max(requestedCartridgeCount - emptySlots, 1) })}</span>
                  <label className="checkbox-inline" style={{ marginTop: 8 }}>
                    <input
                      type="checkbox"
                      checked={cartridgeForm.expandSlots}
                      onChange={(event) =>
                        setCartridgeForm((prev) => ({ ...prev, expandSlots: event.target.checked }))
                      }
                    />
                    <span>{t("resources.addSlotAndInsert")}</span>
                  </label>
                </div>
              ) : null}
              <div className="inline-actions" style={{ gridColumn: "1 / -1", justifyContent: "flex-start" }}>
                <button
                  className="btn btn-quiet"
                  type="button"
                  onClick={() => setCartridgeAdvancedOpen((prev) => !prev)}
                >
                  {cartridgeAdvancedOpen ? t("resources.hideAdvanced") : t("resources.showAdvanced")}
                </button>
              </div>
              {cartridgeAdvancedOpen ? (
                <div className="form-advanced-panel">
                  <div className="form-row">
                    <label>{t("resources.customCartridgeCapacity")}</label>
                    <div className="capacity-input-row">
                      <input
                        className="input"
                        type="number"
                        min={1}
                        step={1}
                        placeholder={formatBytes(tapeProfile.capacityBytes)}
                        value={cartridgeForm.customCapacityValue}
                        onChange={(event) =>
                          setCartridgeForm((prev) => ({ ...prev, customCapacityValue: event.target.value }))
                        }
                      />
                      <SelectInput
                        value={cartridgeForm.customCapacityUnit}
                        onChange={(value) =>
                          setCartridgeForm((prev) => ({
                            ...prev,
                            customCapacityUnit: value as CapacityUnit,
                          }))
                        }
                        options={[
                          { value: "TB", label: "TB" },
                          { value: "GB", label: "GB" },
                          { value: "MB", label: "MB" },
                        ]}
                        ariaLabel={t("resources.capacityUnit")}
                      />
                    </div>
                  </div>
                </div>
              ) : null}
              <div className="inline-actions" style={{ gridColumn: "1 / -1" }}>
                <button className="btn btn-primary" type="submit" disabled={busyCreateCartridge || usablePools.length === 0}>
                  {busyCreateCartridge ? t("common.loading") : t("common.create")}
                </button>
                <button className="btn btn-quiet" type="button" onClick={() => setCreateCartridgeOpen(false)}>{t("common.cancel")}</button>
              </div>
            </form>
          </div>
        </div>
      ) : null}

      <ConfirmDialog
        open={Boolean(deleteTarget)}
        title={deleteDialogTitle}
        message={deleteDialogMessage}
        confirmLabel={deleteTarget?.kind === "cartridge" ? t("resources.destroyCartridge") : t("common.delete")}
        danger
        busy={busyDelete}
        onConfirm={() => void confirmDelete()}
        onCancel={() => {
          if (!busyDelete) {
            setDeleteTarget(null);
          }
        }}
      />
      <ConfirmDialog
        open={Boolean(eraseTarget)}
        title={eraseTarget?.mode === "long" ? t("resources.longEraseTitle") : t("resources.shortEraseTitle")}
        message={t("resources.eraseCartridgeMessage", {
          cartridgeId: eraseTarget?.id || "",
          mode: eraseTarget?.mode === "long" ? t("resources.longErase") : t("resources.shortErase"),
        })}
        confirmLabel={eraseTarget?.mode === "long" ? t("resources.longErase") : t("resources.shortErase")}
        danger
        busy={Boolean(busyResourceAction)}
        onConfirm={() => void confirmErase()}
        onCancel={() => {
          if (!busyResourceAction) {
            setEraseTarget(null);
          }
        }}
      />
      <ConfirmDialog
        open={Boolean(slotShortageImportTarget)}
        title={t("resources.importSlotShortageTitle")}
        message={t("resources.importSlotShortageMessage", {
          cartridgeId: slotShortageImportTarget?.barcode || slotShortageImportTarget?.cartridgeId || "",
        })}
        confirmLabel={t("resources.addSlotAndImport")}
        busy={busyResourceAction.startsWith("add-slot-import:")}
        onConfirm={() => void addSlotAndImportCartridge()}
        onCancel={() => {
          if (!busyResourceAction) {
            setSlotShortageImportTarget(null);
          }
        }}
      />
    </section>
  );
}
