import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../services/api";
import { useToast } from "./Toast";
import { AppModal } from "./AppModal";
import { SelectInput } from "./SelectInput";
import { IscsiSecurityBlockEditor, type SecurityEditorDraft } from "./IscsiSecurityBlockEditor";
import "../styles/iscsi-security-form.css";
import type {
  ISCSICredentialMetadata,
  ISCSISecurityBinding,
  ISCSISecurityImpact,
  ISCSISecurityTargetView,
  TargetPublication,
} from "../services/types";

type Scope = "library" | "drive" | "target";
type AuthSelection = "inherit" | "none" | "chap" | "mutual_chap";

interface Props {
  scope: Scope;
  ownerId: string;
  title: string;
  open?: boolean;
  onClose?: () => void;
  modalOnly?: boolean;
  editorSection?: "chap";
}

function splitInitiators(value: string): string[] {
  return [...new Set(value.split(/[\r\n,]+/).map((item) => item.trim()).filter(Boolean))];
}

function isValidInitiatorIQN(value: string): boolean {
  return value.length <= 223 && !value.includes("..") && /^iqn\.[0-9]{4}-[0-9]{2}\.[a-z0-9][a-z0-9.-]*:[a-z0-9][a-z0-9:._-]*$/i.test(value);
}

function weakensProtection(before: ISCSISecurityImpact["before"], after: ISCSISecurityImpact["after"]): boolean {
  const strength = { none: 0, chap: 1, mutual_chap: 2 };
  if (strength[before.auth.mode] > strength[after.auth.mode]) return true;
  if (before.auth.restrictInitiators && !after.auth.restrictInitiators && after.auth.mode === "none") return true;
  const beforeAllowed = new Set(before.auth.initiators || []);
  const afterAllowed = after.auth.initiators || [];
  const beforeHasFiniteSet = before.auth.mode !== "none" || Boolean(before.auth.restrictInitiators);
  const afterUnrestricted = after.auth.mode === "none" && !after.auth.restrictInitiators;
  if (beforeHasFiniteSet && afterUnrestricted) return true;
  if (beforeHasFiniteSet && afterAllowed.some((initiator) => !beforeAllowed.has(initiator))) return true;
  return false;
}

export function IscsiSecurityForm({ scope, ownerId, title, open, onClose, modalOnly = false, editorSection }: Props) {
  const { t } = useTranslation();
  const { push } = useToast();
  const [binding, setBinding] = useState<ISCSISecurityBinding | null>(null);
  const [targets, setTargets] = useState<ISCSISecurityTargetView[]>([]);
  const [credentials, setCredentials] = useState<ISCSICredentialMetadata[]>([]);
  const [authSelection, setAuthSelection] = useState<AuthSelection>("inherit");
  const [credentialId, setCredentialId] = useState("");
  const [initiators, setInitiators] = useState("");
  const [restrictInitiators, setRestrictInitiators] = useState(false);
  const [preview, setPreview] = useState<ISCSISecurityImpact[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [initiatorError, setInitiatorError] = useState("");
  const [internalOpen, setInternalOpen] = useState(false);
  const controlled = typeof open === "boolean";
  const dialogOpen = controlled ? open : internalOpen;

  const authModeLabel = (mode: string) => mode === "chap"
    ? t("iscsiSecurity.oneWayChap")
    : mode === "mutual_chap"
      ? t("iscsiSecurity.mutualChap")
      : t("iscsiSecurity.authNoneSummary");
  const sourceLabel = (source: string) => {
    switch (source) {
      case "library": return t("iscsiSecurity.sourceLibrary");
      case "drive": return t("iscsiSecurity.sourceDrive");
      case "target": return t("iscsiSecurity.sourceTarget");
      default: return t("iscsiSecurity.sourceDefault");
    }
  };
  const scopeHint = scope === "library"
    ? t("iscsiSecurity.libraryScopeHint")
    : scope === "drive"
      ? t("iscsiSecurity.driveScopeHint")
      : t("iscsiSecurity.targetScopeHint");
  const editorTitle = editorSection === "chap" ? t("resources.configureChapTitle") : title;
  const editorDescription = editorSection === "chap"
    ? scope === "drive"
      ? t("iscsiSecurity.driveChapOverrideHint")
      : scope === "target"
        ? t("iscsiSecurity.targetChapOverrideHint")
        : t("resources.configureChapHint")
    : scopeHint;

  async function reload() {
    setLoading(true);
    setError("");
    try {
      const [current, credentialRows] = await Promise.all([
        scope === "library"
          ? api.iscsiSecurity.getLibraryBinding(ownerId)
          : scope === "drive"
            ? api.iscsiSecurity.getDriveBinding(ownerId)
            : api.iscsiSecurity.getTargetBinding(ownerId),
        api.iscsiSecurity.listCredentials(),
      ]);
      const currentBinding = current.binding;
      const resolvedTargets = "targets" in current ? current.targets : [current];
      setBinding(currentBinding);
      setTargets(resolvedTargets);
      setCredentials(credentialRows);
      setAuthSelection(currentBinding.auth?.mode || "inherit");
      setCredentialId(currentBinding.auth?.credentialId || "");
      setInitiators(currentBinding.auth?.initiators?.join("\n") || "");
      setRestrictInitiators(Boolean(currentBinding.auth?.restrictInitiators));
      setPreview(null);
    } catch (err) {
      setError((err as Error).message || t("messages.requestFailed"));
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    void reload();
  }, [scope, ownerId]);

  const policyBody = useMemo(() => {
    const auth = authSelection === "inherit"
      ? null
      : {
          mode: authSelection,
          ...(authSelection === "none" ? {} : { credentialId }),
          initiators: splitInitiators(initiators),
          restrictInitiators: authSelection === "none" && restrictInitiators,
        };
    return { auth };
  }, [authSelection, credentialId, initiators, restrictInitiators]);

  async function requestPreview() {
    if (!binding) return;
    const body = { generation: (binding.generation || 0) + 1, ...policyBody, actor: "web-console" };
    const response = scope === "library"
      ? await api.iscsiSecurity.previewLibraryBinding(ownerId, body)
      : scope === "drive"
        ? await api.iscsiSecurity.previewDriveBinding(ownerId, body)
        : await api.iscsiSecurity.previewTargetBinding(ownerId, body);
    return response.targets;
  }

  async function save() {
    if (!binding) return;
    if (authSelection !== "inherit" && authSelection !== "none" && !credentialId) {
      setError(t("iscsiSecurity.credentialRequired"));
      return;
    }
    const requiresInitiators = authSelection === "chap" || authSelection === "mutual_chap" || (authSelection === "none" && restrictInitiators);
    const initiatorList = splitInitiators(initiators);
    if (requiresInitiators && initiatorList.length === 0) {
      setInitiatorError(t("iscsiSecurity.initiatorRequired"));
      return;
    }
    if (requiresInitiators && initiatorList.some((initiator) => !isValidInitiatorIQN(initiator))) {
      setInitiatorError(t("iscsiSecurity.initiatorInvalid"));
      return;
    }
    setInitiatorError("");
    setBusy(true);
    setError("");
    try {
      const impact = await requestPreview();
      if (impact?.some((item) => item.changed)) {
        setPreview(impact);
        return;
      }
      await persistBinding();
    } catch (err) {
      setError((err as Error).message || t("messages.requestFailed"));
    } finally {
      setBusy(false);
    }
  }

  async function putBinding() {
    if (!binding) return;
    const body = { generation: (binding.generation || 0) + 1, ...policyBody, actor: "web-console" };
    if (scope === "library") await api.iscsiSecurity.putLibraryBinding(ownerId, body);
    else if (scope === "drive") await api.iscsiSecurity.putDriveBinding(ownerId, body);
    else await api.iscsiSecurity.putTargetBinding(ownerId, body);
  }

  function closeAfterSave() {
    setPreview(null);
    if (controlled) onClose?.();
    else setInternalOpen(false);
  }

  async function persistBinding() {
    await putBinding();
    push(t("messages.requestSuccess"), "success");
    await reload();
    closeAfterSave();
  }

  function republish(publication: TargetPublication) {
    return api.targets.createPublication({
      libraryId: publication.libraryId,
      driveId: publication.driveId,
      cartridgeId: publication.cartridgeId,
      targetIqn: publication.targetIqn,
      deviceRole: publication.deviceRole,
      deviceProfile: publication.deviceProfile,
      driveProfile: publication.driveProfile,
      actor: "web-console",
    });
  }

  async function restorePublications(publications: TargetPublication[]): Promise<string[]> {
    const failed: string[] = [];
    for (const publication of publications) {
      try {
        await republish(publication);
      } catch {
        failed.push(publication.targetIqn);
      }
    }
    return failed;
  }

  async function applyAndRestartTargets() {
    if (!preview) return;
    const changedTargets = new Set(preview.filter((item) => item.changed).map((item) => item.targetIqn));
    const originallyOnline = (await api.targets.listPublications()).filter(
      (publication) => publication.state === "ready" && changedTargets.has(publication.targetIqn),
    );
    const takenOffline: TargetPublication[] = [];

    try {
      for (const publication of originallyOnline) {
        await api.targets.unpublish(publication.publicationId);
        takenOffline.push(publication);
      }
    } catch (err) {
      const restoreFailures = await restorePublications(takenOffline);
      await reload();
      const suffix = restoreFailures.length ? ` ${t("iscsiSecurity.restoreFailedTargets", { targets: restoreFailures.join(", ") })}` : "";
      setError(`${(err as Error).message || t("messages.requestFailed")}${suffix}`);
      return;
    }

    try {
      await putBinding();
    } catch (err) {
      const restoreFailures = await restorePublications(takenOffline);
      await reload();
      const suffix = restoreFailures.length ? ` ${t("iscsiSecurity.restoreFailedTargets", { targets: restoreFailures.join(", ") })}` : "";
      setError(`${(err as Error).message || t("messages.requestFailed")}${suffix}`);
      return;
    }

    const onlineFailures = await restorePublications(takenOffline);
    await reload();
    if (onlineFailures.length) {
      push(t("iscsiSecurity.savedButTargetsOffline", { targets: onlineFailures.join(", ") }), "error");
      closeAfterSave();
      return;
    }
    push(t("iscsiSecurity.savedAndTargetsRestarted", { count: takenOffline.length }), "success");
    closeAfterSave();
  }

  async function confirmSave() {
    setBusy(true);
    setError("");
    try {
      if (preview?.some((item) => item.busy)) await applyAndRestartTargets();
      else await persistBinding();
    } catch (err) {
      setError((err as Error).message || t("messages.requestFailed"));
    } finally {
      setBusy(false);
    }
  }

  function cancelPreview() {
    setPreview(null);
  }

  function closeEditor() {
    setPreview(null);
    setError("");
    setInitiatorError("");
    if (controlled) onClose?.();
    else {
      setInternalOpen(false);
      void reload();
    }
  }

  return (
    <>
      {!modalOnly ? (
        <section className="panel iscsi-security-summary">
          <div className="iscsi-security-summary-header">
            <div>
              <h3>{title}</h3>
              <p>{scopeHint}</p>
            </div>
            <button className="btn btn-quiet" type="button" disabled={loading} onClick={() => setInternalOpen(true)}>
              {t("iscsiSecurity.configure")}
            </button>
          </div>
          {loading ? <p className="iscsi-security-loading">{t("common.loading")}</p> : error && !dialogOpen ? <p className="notice notice-error">{error}</p> : targets.length ? (
            <div className="iscsi-security-summary-status">
              <div><span>{t("iscsiSecurity.authentication")}</span><strong>{authModeLabel(targets[0].resolved.auth.mode)}</strong></div>
            </div>
          ) : <p className="iscsi-security-loading">{t("iscsiSecurity.noTargets")}</p>}
        </section>
      ) : null}

      <AppModal open={dialogOpen} title={editorTitle} description={editorDescription} size="medium" busy={busy} onClose={closeEditor}>
        {loading ? <p className="notice">{t("common.loading")}</p> : (
          <div className={editorSection ? "iscsi-security-create-layout" : "iscsi-security-dialog"}>
            {error ? <p className="notice notice-error" role="alert">{error}</p> : null}
            {!editorSection && scope !== "drive" && targets.length ? (
              <details className="iscsi-security-targets">
                <summary>{t("iscsiSecurity.affectedTargets", { count: targets.length })}</summary>
                <div className="iscsi-security-sources">
                  {targets.map((target) => (
                    <div key={target.binding.targetIqn || target.binding.ownerId} className="inspector-field">
                      <span>{target.binding.targetIqn || target.binding.ownerId}</span>
                      <strong>
                        {t("iscsiSecurity.effectiveAuth", { mode: authModeLabel(target.resolved.auth.mode), source: sourceLabel(target.resolved.authSource.scope) })}
                      </strong>
                    </div>
                  ))}
                </div>
              </details>
            ) : !editorSection && scope !== "drive" ? <p className="iscsi-security-loading">{t("iscsiSecurity.noTargets")}</p> : null}

            <form className={editorSection ? "form-grid iscsi-security-editor-form" : undefined} onSubmit={(event) => { event.preventDefault(); void save(); }}>
              {editorSection ? (
                <IscsiSecurityBlockEditor
                  scope={scope}
                  draft={{
                    authMode: authSelection,
                    credentialId,
                    initiators,
                  }}
                  credentials={credentials}
                  loading={loading}
                  onRefresh={() => void reload()}
                  restrictInitiators={restrictInitiators}
                  onRestrictInitiatorsChange={setRestrictInitiators}
                  onChange={(draft: SecurityEditorDraft) => {
                    setAuthSelection(draft.authMode);
                    setCredentialId(draft.credentialId);
                    setInitiators(draft.initiators);
                    setError("");
                  }}
                />
              ) : (
                <>
              <div className="form-row">
                <label>{t("iscsiSecurity.authentication")}</label>
                <SelectInput value={authSelection} ariaLabel={t("iscsiSecurity.authentication")} onChange={(value) => { setAuthSelection(value as AuthSelection); setCredentialId(""); setError(""); }} options={[
                  { value: "inherit", label: t("iscsiSecurity.inherit") },
                  { value: "none", label: t("iscsiSecurity.noChap") },
                  { value: "chap", label: "CHAP" },
                  { value: "mutual_chap", label: t("iscsiSecurity.mutualChap") },
                ]} />
              </div>
              {authSelection !== "inherit" ? (
                <>
                  {authSelection !== "none" ? (
                    <div className="form-row">
                      <label>{t("iscsiSecurity.credential")}</label>
                      <SelectInput value={credentialId} ariaLabel={t("iscsiSecurity.credential")} required onChange={(value) => { setCredentialId(value); setError(""); }} options={[
                        { value: "", label: t("common.noSelection") },
                        ...credentials.filter((item) => authSelection !== "mutual_chap" || item.mutualUsername).map((item) => ({ value: item.credentialId, label: `${item.label} · ${item.username}` })),
                      ]} />
                      {authSelection === "mutual_chap" ? <small>{t("iscsiSecurity.mutualClientHint")}</small> : null}
                    </div>
                  ) : null}
                  <div className="form-row">
                    <label htmlFor="iscsi-initiators">
                      {t("iscsiSecurity.allowedInitiators")}
                      {authSelection === "chap" || authSelection === "mutual_chap" || restrictInitiators ? <span aria-hidden="true"> *</span> : null}
                    </label>
                    <textarea
                      id="iscsi-initiators"
                      className="input"
                      value={initiators}
                      onChange={(event) => { setInitiators(event.target.value); setInitiatorError(""); }}
                      rows={3}
                      placeholder="iqn.1991-05.com.microsoft:initiator"
                      aria-required={authSelection === "chap" || authSelection === "mutual_chap" || (authSelection === "none" && restrictInitiators)}
                      aria-invalid={Boolean(initiatorError)}
                      aria-describedby={`iscsi-initiators-hint${initiatorError ? " iscsi-initiators-error" : ""}`}
                    />
                    <small id="iscsi-initiators-hint">{t("iscsiSecurity.initiatorHint")}</small>
                    {initiatorError ? <p className="notice notice-error" id="iscsi-initiators-error" role="alert">{initiatorError}</p> : null}
                  </div>
                  {authSelection === "none" ? (
                    <label className="cdb-trace-toggle security-restriction-toggle">
                      <input type="checkbox" checked={restrictInitiators} onChange={(event) => setRestrictInitiators(event.target.checked)} />
                      <span className="switch-track" aria-hidden="true"><span className="switch-thumb" /></span>
                      <span className="switch-label">{t("iscsiSecurity.restrictInitiators")}</span>
                    </label>
                  ) : null}
                </>
              ) : null}

                </>
              )}
              {preview ? (
                <div className="notice">
                  <strong>{t("iscsiSecurity.previewTitle")}</strong>
                  {preview.map((item) => (
                    <p key={item.targetIqn}>
                      {item.targetIqn}: {t("iscsiSecurity.authChangeSummary", { before: authModeLabel(item.before.auth.mode), after: authModeLabel(item.after.auth.mode) })}; {item.busy ? t("iscsiSecurity.willRestart") : item.administrativeOffline && item.absent ? t("iscsiSecurity.offline") : t("iscsiSecurity.online")}
                      {weakensProtection(item.before, item.after) ? ` · ${t("iscsiSecurity.weakerProtection")}` : ""}
                    </p>
                  ))}
                  {preview.some((item) => item.busy) ? <p>{t("iscsiSecurity.restartWarning")}</p> : null}
                  <div className={`inline-actions${editorSection ? " form-row-wide resource-create-actions resource-create-main-actions" : ""}`}>
                    <button className="btn btn-primary" type="button" disabled={busy} onClick={() => void confirmSave()}>
                      {busy ? t("common.loading") : preview.some((item) => item.busy) ? t("iscsiSecurity.saveAndRestart") : t("iscsiSecurity.confirmChange")}
                    </button>
                    <button className="btn btn-quiet" type="button" disabled={busy} onClick={cancelPreview}>{t("common.cancel")}</button>
                  </div>
                </div>
              ) : null}
              {!preview ? (
                <div className={`inline-actions${editorSection ? " form-row-wide resource-create-actions resource-create-main-actions" : ""}`}>
                  <button className="btn btn-primary" type="submit" disabled={busy || loading}>{editorSection ? t("resources.saveSecuritySettings") : t("common.save")}</button>
                  <button className="btn btn-quiet" type="button" disabled={busy} onClick={closeEditor}>{t("common.cancel")}</button>
                </div>
              ) : null}
            </form>
          </div>
        )}
      </AppModal>
    </>
  );
}
