import { FormEvent, useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Plus } from "lucide-react";
import { api } from "../services/api";
import { AppModal } from "../components/AppModal";
import { IscsiSecurityBlockEditor, type SecurityEditorDraft } from "../components/IscsiSecurityBlockEditor";
import { useToast } from "../components/Toast";
import { SelectInput } from "../components/SelectInput";
import type { ISCSICredentialMetadata, VirtualCartridge, VirtualDrive, VirtualLibrary } from "../services/types";
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

type CreationSecurityDraft = SecurityEditorDraft;
type SecurityEditor = "chap";

const DEFAULT_CREATION_SECURITY: CreationSecurityDraft = {
  authMode: "none",
  credentialId: "",
  initiators: "",
};

function splitInitiatorIQNs(value: string): string[] {
  return [...new Set(value.split(/[\r\n,]+/).map((item) => item.trim()).filter(Boolean))];
}

function validInitiatorIQN(value: string): boolean {
  return value.length <= 223 && !value.includes("..") && /^iqn\.[0-9]{4}-[0-9]{2}\.[a-z0-9][a-z0-9.-]*:[a-z0-9][a-z0-9:._-]*$/i.test(value);
}

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
  const [createDialogTopOffset, setCreateDialogTopOffset] = useState<number>();
  const [createDialogHeight, setCreateDialogHeight] = useState<number>();
  const [securityEditor, setSecurityEditor] = useState<SecurityEditor | null>(null);
  const [securityEditorDraft, setSecurityEditorDraft] = useState<CreationSecurityDraft>(DEFAULT_CREATION_SECURITY);
  const [securityMaterialLoading, setSecurityMaterialLoading] = useState(false);
  const [securityMaterialError, setSecurityMaterialError] = useState("");
  const [createError, setCreateError] = useState("");
  const [credentials, setCredentials] = useState<ISCSICredentialMetadata[]>([]);
  const [creationSecurity, setCreationSecurity] = useState<CreationSecurityDraft>(DEFAULT_CREATION_SECURITY);

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

  async function loadSecurityChoices() {
    setSecurityMaterialLoading(true);
    setSecurityMaterialError("");
    try {
      const credentialRows = await api.iscsiSecurity.listCredentials();
      setCredentials(credentialRows);
    } catch (err) {
      setSecurityMaterialError((err as Error).message || t("messages.requestFailed"));
    } finally {
      setSecurityMaterialLoading(false);
    }
  }

  function closeVtlDialog() {
    if (creating) return;
    setVtlDialogOpen(false);
    setCreateDialogTopOffset(undefined);
    setCreateDialogHeight(undefined);
    setSecurityEditor(null);
    setSecurityEditorDraft(DEFAULT_CREATION_SECURITY);
    setSecurityMaterialError("");
    setCreateError("");
    setCreationSecurity(DEFAULT_CREATION_SECURITY);
  }

  function pinCreateDialogPosition() {
    const dialog = document.querySelector<HTMLElement>(".app-modal-card");
    if (!dialog) return;
    const bounds = dialog.getBoundingClientRect();
    if (bounds.height <= 0) return;
    setCreateDialogTopOffset(bounds.top);
    setCreateDialogHeight(bounds.height);
  }

  function openSecurityEditor(editor: SecurityEditor) {
    pinCreateDialogPosition();
    setSecurityEditorDraft({ ...creationSecurity });
    setSecurityEditor(editor);
    setCreateError("");
    void loadSecurityChoices();
  }

  function saveSecurityEditor() {
    setCreationSecurity({ ...securityEditorDraft });
    setSecurityEditor(null);
    setCreateError("");
  }

  function cancelSecurityEditor() {
    setSecurityEditorDraft({ ...creationSecurity });
    setSecurityEditor(null);
    setCreateError("");
  }

  async function validateCreationSecurity() {
    const { authMode, credentialId, initiators } = creationSecurity;
    if (authMode === "none") return;
    const initiatorList = splitInitiatorIQNs(initiators);
    if (authMode !== "inherit" && !credentialId) throw new Error(t("iscsiSecurity.credentialRequired"));
    if ((authMode === "chap" || authMode === "mutual_chap") && (initiatorList.length === 0 || initiatorList.some((iqn) => !validInitiatorIQN(iqn)))) {
      throw new Error(t("iscsiSecurity.initiatorInvalid"));
    }
  }

  async function saveCreationSecurity(libraryId: string) {
    const { authMode, credentialId, initiators } = creationSecurity;
    if (authMode === "none") return;
    const initiatorList = splitInitiatorIQNs(initiators);
    await api.iscsiSecurity.putLibraryBinding(libraryId, {
      generation: 1,
      auth: authMode === "inherit" ? null : { mode: authMode, credentialId, initiators: initiatorList },
      actor: "web-console",
    });
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

    setCreateError("");
    try {
      await validateCreationSecurity();
    } catch (err) {
      setCreateError((err as Error).message || t("messages.requestFailed"));
      pinCreateDialogPosition();
      setSecurityEditorDraft({ ...creationSecurity });
      setSecurityEditor("chap");
      void loadSecurityChoices();
      return;
    }

    setCreating(true);
    let securitySaveError: unknown;
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

      try {
        await saveCreationSecurity(libraryId);
      } catch (err) {
        securitySaveError = err;
      }

      if (securitySaveError) {
        push(t("resources.createSecuritySaveFailed", { message: (securitySaveError as Error).message || t("messages.requestFailed") }), "error");
      } else {
        push(t("messages.requestSuccess"), "success");
      }
      setVtlDialogOpen(false);
      setCreateDialogTopOffset(undefined);
      setCreateDialogHeight(undefined);
      setSecurityEditor(null);
      setSecurityEditorDraft(DEFAULT_CREATION_SECURITY);
      setSecurityMaterialError("");
      setCreateError("");
      setCreationSecurity(DEFAULT_CREATION_SECURITY);
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

  const creationInitiators = splitInitiatorIQNs(creationSecurity.initiators);
  const chapNeedsSetup = creationSecurity.authMode !== "none" && creationSecurity.authMode !== "inherit" && (
    !creationSecurity.credentialId ||
    creationInitiators.length === 0 ||
    creationInitiators.some((iqn) => !validInitiatorIQN(iqn))
  );
  const chapStatus = creationSecurity.authMode === "none" || creationSecurity.authMode === "inherit"
    ? t("resources.notConfigured")
    : t(chapNeedsSetup ? "resources.needsSetup" : "resources.configured");
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
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      {vtlDialogOpen ? (
        <AppModal
          open={vtlDialogOpen}
          title={securityEditor === "chap"
            ? t("resources.configureChapTitle")
            : t("resources.createVtlDialogTitle")}
          description={securityEditor === "chap"
            ? t("resources.configureChapHint")
            : undefined}
          size="medium"
          topOffset={createDialogTopOffset}
          fixedHeight={createDialogHeight}
          focusKey={securityEditor ?? "create"}
          busy={creating}
          onClose={securityEditor ? cancelSecurityEditor : closeVtlDialog}
        >
          {securityEditor ? (
            <form
              className="form-grid resource-create-security-editor"
              onSubmit={(event) => {
                event.preventDefault();
                saveSecurityEditor();
              }}
            >
              {createError ? <p className="notice notice-error form-row-wide" role="alert">{createError}</p> : null}
              {securityMaterialError ? <p className="notice notice-error form-row-wide" role="alert">{securityMaterialError}</p> : null}
              {securityMaterialLoading ? <p className="notice form-row-wide">{t("common.loading")}</p> : null}
              {securityMaterialError ? (
                <div className="inline-actions form-row-wide">
                  <a className="btn btn-quiet" href="/ui/security" target="_blank" rel="noreferrer">{t("resources.openConnectionSecurity")}</a>
                  <button className="btn btn-quiet" type="button" disabled={securityMaterialLoading} onClick={() => void loadSecurityChoices()}>{t("resources.refreshSecurityOptions")}</button>
                </div>
              ) : null}
              <IscsiSecurityBlockEditor
                scope="create"
                draft={securityEditorDraft}
                credentials={credentials}
                loading={securityMaterialLoading}
                onRefresh={() => void loadSecurityChoices()}
                onChange={setSecurityEditorDraft}
              />
              <div className="inline-actions form-row-wide resource-create-actions resource-create-main-actions" style={{ justifyContent: "flex-end" }}>
                <button className="btn btn-primary" type="submit" disabled={creating}>
                  {t("resources.saveSecuritySettings")}
                </button>
                <button className="btn btn-quiet" type="button" disabled={creating} onClick={cancelSecurityEditor}>
                  {t("common.cancel")}
                </button>
              </div>
            </form>
          ) : (
            <form className="form-grid resource-create-main-form" onSubmit={createVtl}>
              <div className="form-row">
                <label htmlFor="create-vtl-name">{t("resources.vtlName")}</label>
                <input
                  id="create-vtl-name"
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
              <div className="resource-security-setup-grid form-row-wide">
                <div className="resource-security-config-row">
                  <div className="resource-security-config-heading">
                    <h3>{t("resources.chapTitle")}</h3>
                    <span className={`resource-security-config-status${chapNeedsSetup ? " is-incomplete" : creationSecurity.authMode !== "none" ? " is-configured" : ""}`}>
                      {chapStatus}
                    </span>
                  </div>
                  <button className="btn btn-quiet" type="button" disabled={creating} onClick={() => openSecurityEditor("chap")}>
                    {t("resources.configureChap")}
                  </button>
                </div>
              </div>
              <div className="inline-actions form-row-wide resource-create-actions resource-create-main-actions" style={{ justifyContent: "flex-end" }}>
                <button className="btn btn-primary" type="submit" disabled={creating || securityMaterialLoading}>
                  {creating ? t("common.loading") : t("common.create")}
                </button>
                <button className="btn btn-quiet" type="button" disabled={creating} onClick={closeVtlDialog}>
                  {t("common.cancel")}
                </button>
              </div>
            </form>
          )}
        </AppModal>
      ) : null}

    </section>
  );
}
