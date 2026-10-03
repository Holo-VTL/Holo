import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useTranslation } from "react-i18next";
import { CheckCircle2, CircleAlert, LoaderCircle, MoreHorizontal, RefreshCw } from "lucide-react";
import { api } from "../services/api";
import { useToast } from "../components/Toast";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { StatusBadge } from "../components/StatusBadge";
import { IscsiSecurityForm } from "../components/IscsiSecurityForm";
import type { ConnectedHostsSummary, LocalMountStatus, TargetPublication } from "../services/types";
import type { ISCSISecurityTargetView } from "../services/types";

type PublicationAction = { kind: "offline" | "online"; publication: TargetPublication };
type SelectedSecurityEditor = { targetIqn: string; section: "chap" };
type TargetRow = { targetIqn: string; deviceRole: string; publication?: TargetPublication };

interface TargetActionsMenuProps {
  targetIqn: string;
  publication?: TargetPublication;
  busy: boolean;
  onConfigure: (section: "chap") => void;
  onPublicationAction: (action: PublicationAction) => void;
}

function TargetActionsMenu({ targetIqn, publication, busy, onConfigure, onPublicationAction }: TargetActionsMenuProps) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [position, setPosition] = useState({ top: 0, left: 0 });
  const triggerRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);

  useLayoutEffect(() => {
    if (!open) return;
    const updatePosition = () => {
      const trigger = triggerRef.current;
      const menu = menuRef.current;
      if (!trigger || !menu) return;
      const triggerRect = trigger.getBoundingClientRect();
      const menuRect = menu.getBoundingClientRect();
      const openAbove = window.innerHeight - triggerRect.bottom < menuRect.height && triggerRect.top > window.innerHeight - triggerRect.bottom;
      const top = openAbove ? triggerRect.top - menuRect.height - 4 : triggerRect.bottom + 4;
      const left = Math.max(8, Math.min(triggerRect.right - menuRect.width, window.innerWidth - menuRect.width - 8));
      setPosition({ top, left });
    };
    updatePosition();
    window.addEventListener("resize", updatePosition);
    window.addEventListener("scroll", updatePosition, true);
    return () => {
      window.removeEventListener("resize", updatePosition);
      window.removeEventListener("scroll", updatePosition, true);
    };
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const closeOnOutsidePointerDown = (event: PointerEvent) => {
      const target = event.target as Node;
      if (!triggerRef.current?.contains(target) && !menuRef.current?.contains(target)) setOpen(false);
    };
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setOpen(false);
        triggerRef.current?.focus();
      }
    };
    document.addEventListener("pointerdown", closeOnOutsidePointerDown);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", closeOnOutsidePointerDown);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [open]);

  const runConfigure = (section: "chap") => {
    setOpen(false);
    onConfigure(section);
  };

  const runPublicationAction = (kind: PublicationAction["kind"]) => {
    setOpen(false);
    if (publication) onPublicationAction({ kind, publication });
  };

  const isReady = publication?.state === "ready";
  const isDisabled = publication?.state === "disabled";
  const menu = open ? createPortal(
    <div ref={menuRef} className="target-actions-menu" role="menu" style={{ top: position.top, left: position.left, visibility: position.top === 0 ? "hidden" : "visible" }}>
      <button type="button" role="menuitem" onClick={() => runConfigure("chap")}>{t("targets.setChap")}</button>
      <div className="target-actions-menu-separator" />
      <button type="button" role="menuitem" disabled={busy || (!isReady && !isDisabled)} onClick={() => runPublicationAction(isDisabled ? "online" : "offline")}>
        {t(isDisabled ? "iscsiSecurity.bringTargetOnline" : "iscsiSecurity.takeTargetOffline")}
      </button>
    </div>,
    document.body,
  ) : null;

  return (
    <>
      <button
        ref={triggerRef}
        className="icon-btn target-actions-trigger"
        type="button"
        aria-label={t("targets.actionsForTarget", { targetIqn })}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((value) => !value)}
      >
        <MoreHorizontal size={17} />
      </button>
      {menu}
    </>
  );
}

function makeTargetRows(publications: TargetPublication[], securityTargets: ISCSISecurityTargetView[]): TargetRow[] {
  const rows = new Map<string, TargetRow>();
  for (const target of securityTargets) {
    const targetIqn = target.binding.targetIqn || "";
    if (!targetIqn) continue;
    rows.set(targetIqn, {
      targetIqn,
      deviceRole: target.binding.deviceRole || "drive",
      publication: publicationForTarget(publications, targetIqn),
    });
  }
  for (const publication of publications) {
    if (publication.state !== "ready" && publication.state !== "disabled") continue;
    const row = rows.get(publication.targetIqn);
    if (row) row.publication = publication;
    else rows.set(publication.targetIqn, { targetIqn: publication.targetIqn, deviceRole: publication.deviceRole, publication });
  }
  return [...rows.values()].sort((left, right) => left.targetIqn.localeCompare(right.targetIqn));
}

function publicationForTarget(publications: TargetPublication[], targetIqn: string): TargetPublication | undefined {
  const matches = publications.filter((publication) => publication.targetIqn === targetIqn);
  return matches.find((publication) => publication.state === "ready")
    || matches.sort((left, right) => Date.parse(right.updatedAt) - Date.parse(left.updatedAt))[0];
}

function connectedHostsTitle(connectedHosts?: ConnectedHostsSummary): string | undefined {
  if (!connectedHosts?.available || connectedHosts.initiators.length === 0) {
    return undefined;
  }
  return connectedHosts.initiators.join("\n");
}

interface ConnectedHostsLabels {
  activeHosts: (count: number) => string;
  noActiveSessions: string;
  sessionDataUnavailable: string;
}

function renderConnectedHosts(connectedHosts: ConnectedHostsSummary | undefined, labels: ConnectedHostsLabels) {
  if (!connectedHosts || !connectedHosts.available) {
    return <span className="connected-hosts-value connected-hosts-value-muted">{labels.sessionDataUnavailable}</span>;
  }
  if (connectedHosts.hostCount === 0) {
    return <span className="connected-hosts-value connected-hosts-value-muted">{labels.noActiveSessions}</span>;
  }
  return (
    <div className="connected-hosts-cell" title={connectedHostsTitle(connectedHosts)}>
      <span className="connected-hosts-value">{labels.activeHosts(connectedHosts.hostCount)}</span>
    </div>
  );
}

export function TargetsPage() {
  const { t } = useTranslation();
  const { push } = useToast();
  const [publications, setPublications] = useState<TargetPublication[]>([]);
  const [securityTargets, setSecurityTargets] = useState<ISCSISecurityTargetView[]>([]);
  const [selectedSecurityEditor, setSelectedSecurityEditor] = useState<SelectedSecurityEditor | null>(null);
  const [localMount, setLocalMount] = useState<LocalMountStatus | null>(null);
  const [error, setError] = useState("");
  const [mountBusy, setMountBusy] = useState(false);
  const [mountStatusUnavailable, setMountStatusUnavailable] = useState(false);
  const mountPollInFlight = useRef(false);
  const mountRequestInFlight = useRef(false);
  const mountOperation = useRef<boolean | null>(null);
  const mountStatusRevision = useRef(0);
  const [publicationAction, setPublicationAction] = useState<PublicationAction | null>(null);
  const [publicationActionBusy, setPublicationActionBusy] = useState(false);

  const securityEnforcementLabel = (state?: string) => {
    switch (state) {
      case "unprotected": return t("iscsiSecurity.enforcementUnprotected");
      case "simulated": return t("iscsiSecurity.enforcementSimulated");
      case "enforcing": return t("iscsiSecurity.enforcementEnforcing");
      case "blocked": return t("iscsiSecurity.enforcementBlocked");
      case "offline": return t("iscsiSecurity.enforcementOffline");
      default: return t("iscsiSecurity.enforcementUnknown");
    }
  };
  const deviceRoleLabel = (role: string) => role === "changer"
    ? t("iscsiSecurity.roleChanger")
    : t("iscsiSecurity.roleDrive");

  const applyMountStatus = useCallback((status: LocalMountStatus) => {
    setLocalMount(status);
    setMountStatusUnavailable(false);
    const operation = mountOperation.current;
    if (operation === null) return;
    if (status.enabled !== operation) {
      mountOperation.current = null;
      return;
    }
    if (status.state === "connecting" || status.state === "disconnecting") return;
    mountOperation.current = null;
    const completed = operation ? status.state === "connected" : status.state === "disabled" && status.residualDeviceCount === 0;
    const message = completed
      ? t(operation ? "targets.localMountCompleted" : "targets.localMountDisabled")
      : t(!operation ? "targets.localMountStates.removalFailed" : status.state === "partial" ? "targets.localMountPartialFailure" : "targets.localMountStates.failed");
    push(message, completed ? "success" : "error");
  }, [push, t]);

  const refreshMountStatus = useCallback(async () => {
    if (mountPollInFlight.current || mountRequestInFlight.current) return;
    mountPollInFlight.current = true;
    const revision = mountStatusRevision.current;
    try {
      const status = await api.targets.localMountStatus();
      if (revision === mountStatusRevision.current && !mountRequestInFlight.current) applyMountStatus(status);
    } catch {
      if (revision === mountStatusRevision.current && !mountRequestInFlight.current) setMountStatusUnavailable(true);
    } finally {
      mountPollInFlight.current = false;
    }
  }, [applyMountStatus]);

  async function reloadAll() {
    setError("");
    try {
      const [pubRows, securityRows] = await Promise.all([
        api.targets.listPublications(),
        api.iscsiSecurity.listTargets(),
        refreshMountStatus(),
      ]);
      setPublications(pubRows);
      setSecurityTargets(securityRows);
      setSelectedSecurityEditor((current) => current && securityRows.some((row) => row.binding.targetIqn === current.targetIqn) ? current : null);
    } catch (err) {
      setError((err as Error).message || t("messages.apiError"));
    }
  }

  async function toggleLocalMount(enabled: boolean) {
    if (mountRequestInFlight.current || mountOperation.current !== null) return;
    mountRequestInFlight.current = true;
    mountStatusRevision.current++;
    mountOperation.current = enabled;
    setMountBusy(true);
    let recheck = false;
    try {
      const status = await api.targets.setLocalMount(enabled);
      applyMountStatus(status);
    } catch (err) {
      mountOperation.current = null;
      setMountStatusUnavailable(true);
      recheck = true;
      push((err as Error).message || t("messages.requestFailed"), "error");
    } finally {
      mountRequestInFlight.current = false;
      setMountBusy(false);
    }
    if (recheck) await refreshMountStatus();
  }

  async function confirmPublicationAction() {
    if (!publicationAction) return;
    setPublicationActionBusy(true);
    try {
      const { kind, publication } = publicationAction;
      if (kind === "offline") {
        await api.targets.unpublish(publication.publicationId);
      } else {
        await api.targets.createPublication({
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
      await reloadAll();
      push(t(kind === "offline" ? "iscsiSecurity.targetOfflineSuccess" : "iscsiSecurity.targetOnlineSuccess"), "success");
      setPublicationAction(null);
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    } finally {
      setPublicationActionBusy(false);
    }
  }

  useEffect(() => {
    void reloadAll();
  }, []);
  useEffect(() => {
    const timer = window.setInterval(() => { void refreshMountStatus(); }, 2500);
    return () => window.clearInterval(timer);
  }, [refreshMountStatus]);
  const targetRows = makeTargetRows(publications, securityTargets);
  const mountProcessing = mountBusy || localMount?.state === "connecting" || localMount?.state === "disconnecting";
  const mountFailed = localMount?.state === "partial" || localMount?.state === "failed";
  const mountState = mountBusy
    ? mountOperation.current ? "connecting" : "disconnecting"
    : mountStatusUnavailable ? "unavailable" : !localMount ? "checking" : localMount.state === "failed" && !localMount.enabled ? "removalFailed" : localMount.state;
  const mountFailureReasons = (localMount?.devices ?? [])
    .filter((device) => device.state !== "connected" && device.reasonCode)
    .map((device) => `${device.displayName}: ${t(`targets.localMountReasons.${device.reasonCode}`, { defaultValue: t("targets.localMountReasons.operation_failed") })}`);
  if (localMount?.lastError) {
    mountFailureReasons.unshift(t(`targets.localMountReasons.${localMount.lastError}`, { defaultValue: t("targets.localMountReasons.operation_failed") }));
  }

  const connectedHostsLabels = {
    activeHosts: (count: number) => t("targets.activeHosts", { count }),
    noActiveSessions: t("targets.noActiveSessionsShort"),
    sessionDataUnavailable: t("targets.sessionDataUnavailableShort"),
  };

  return (
    <section>
      <div className="page-header">
        <div className="targets-page-head">
          <h1 className="page-title">{t("targets.title")}</h1>
          <div className="local-mount-controls">
            <span className={`local-mount-status${mountFailed || mountStatusUnavailable ? " local-mount-status-error" : ""}`} role="status" aria-live="polite">
              {mountStatusUnavailable ? <CircleAlert size={15} aria-hidden="true" />
                : mountProcessing || !localMount ? <LoaderCircle size={15} className="local-mount-spinner" aria-hidden="true" />
                : mountFailed ? <CircleAlert size={15} aria-hidden="true" />
                : localMount.state === "connected" ? <CheckCircle2 size={15} aria-hidden="true" /> : null}
              {t(`targets.localMountStates.${mountState}`)}
            </span>
            <label className="cdb-trace-toggle local-mount-toggle">
              <input
                type="checkbox"
                checked={mountBusy && mountOperation.current !== null ? mountOperation.current : Boolean(localMount?.enabled)}
                disabled={mountProcessing || !localMount || mountStatusUnavailable}
                onChange={(event) => void toggleLocalMount(event.target.checked)}
              />
              <span className="switch-track" aria-hidden="true">
                <span className="switch-thumb" />
              </span>
              <span className="switch-label">{t("targets.mountLocally")}</span>
            </label>
          </div>
        </div>
        {mountStatusUnavailable || mountFailed ? (
          <div className="local-mount-feedback" role="alert">
            <span>{mountStatusUnavailable ? t("targets.localMountStatusUnavailable") : [...new Set(mountFailureReasons)].join(" ") || t("targets.localMountReasons.operation_failed")}</span>
            <button className="btn btn-quiet" type="button" disabled={mountBusy} onClick={() => void (mountStatusUnavailable ? refreshMountStatus() : toggleLocalMount(Boolean(localMount?.enabled)))}>
              <RefreshCw size={14} aria-hidden="true" />
              {t(mountStatusUnavailable ? "targets.localMountCheckAgain" : "targets.localMountRetry")}
            </button>
          </div>
        ) : null}
      </div>

      {error ? <p className="notice notice-error">{error}</p> : null}

      <div className="panel" style={{ marginTop: 12 }}>
        <div className="inline-actions" style={{ justifyContent: "space-between", alignItems: "center", marginBottom: 10 }}>
          <h3 style={{ margin: 0 }}>{t("targets.title")}</h3>
          <button className="btn btn-quiet" type="button" onClick={() => void reloadAll()}>
            <RefreshCw size={14} />
            {t("common.refresh")}
          </button>
        </div>
        <div className="table-wrap">
          <table className="table target-publications-table">
            <thead>
              <tr>
                <th>{t("targets.targetIqn")}</th>
                <th>{t("targets.deviceRole")}</th>
                <th>{t("targets.portal")}</th>
                <th className="connected-hosts-column">{t("targets.connectedHosts")}</th>
                <th>{t("iscsiSecurity.enforcement")}</th>
                <th>{t("common.state")}</th>
                <th>{t("common.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {targetRows.map((row) => (
                <tr key={row.targetIqn}>
                  <td title={row.targetIqn}>{row.targetIqn}</td>
                  <td>{deviceRoleLabel(row.deviceRole)}</td>
                  <td>{row.publication?.portal || "-"}</td>
                  <td className="connected-hosts-column">{renderConnectedHosts(row.publication?.connectedHosts, connectedHostsLabels)}</td>
                  <td>{row.publication ? securityEnforcementLabel(row.publication.securityEnforcement) : t("iscsiSecurity.enforcementOffline")}</td>
                  <td>{row.publication ? <StatusBadge state={row.publication.state} /> : t("iscsiSecurity.online")}</td>
                  <td>
                    <TargetActionsMenu
                      targetIqn={row.targetIqn}
                      publication={row.publication}
                      busy={publicationActionBusy}
                      onConfigure={(section) => setSelectedSecurityEditor({ targetIqn: row.targetIqn, section })}
                      onPublicationAction={setPublicationAction}
                    />
                  </td>
                </tr>
              ))}
              {targetRows.length === 0 ? (
                <tr>
                  <td colSpan={7}>{t("common.empty")}</td>
                </tr>
              ) : null}
            </tbody>
          </table>
        </div>
      </div>

      {selectedSecurityEditor ? (
        <IscsiSecurityForm
          key={`target-${selectedSecurityEditor.targetIqn}-${selectedSecurityEditor.section}`}
          scope="target"
          ownerId={selectedSecurityEditor.targetIqn}
          title={t("iscsiSecurity.targetOverride")}
          editorSection={selectedSecurityEditor.section}
          open
          modalOnly
          onClose={() => setSelectedSecurityEditor(null)}
        />
      ) : null}
      <ConfirmDialog
        open={Boolean(publicationAction)}
        title={t(publicationAction?.kind === "offline" ? "iscsiSecurity.takeTargetOfflineTitle" : "iscsiSecurity.bringTargetOnlineTitle")}
        message={t(publicationAction?.kind === "offline" ? "iscsiSecurity.takeTargetOfflineMessage" : "iscsiSecurity.bringTargetOnlineMessage", { targetIqn: publicationAction?.publication.targetIqn || "" })}
        confirmLabel={t(publicationAction?.kind === "offline" ? "iscsiSecurity.takeTargetOffline" : "iscsiSecurity.bringTargetOnline")}
        danger={publicationAction?.kind === "offline"}
        busy={publicationActionBusy}
        onConfirm={() => void confirmPublicationAction()}
        onCancel={() => { if (!publicationActionBusy) setPublicationAction(null); }}
      />
    </section>
  );
}
