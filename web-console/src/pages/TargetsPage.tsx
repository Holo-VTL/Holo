import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { RefreshCw } from "lucide-react";
import { api } from "../services/api";
import { useToast } from "../components/Toast";
import { StatusBadge } from "../components/StatusBadge";
import type { ConnectedHostsSummary, LocalMountStatus, TargetPublication } from "../services/types";

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
  const [localMount, setLocalMount] = useState<LocalMountStatus | null>(null);
  const [error, setError] = useState("");
  const [mountBusy, setMountBusy] = useState(false);

  async function reloadAll() {
    setError("");
    try {
      const [pubRows, mountStatus] = await Promise.all([
        api.targets.listPublications(),
        api.targets.localMountStatus(),
      ]);
      setPublications(pubRows);
      setLocalMount(mountStatus);
    } catch (err) {
      setError((err as Error).message || t("messages.apiError"));
    }
  }

  async function toggleLocalMount(enabled: boolean) {
    setMountBusy(true);
    try {
      const status = await api.targets.setLocalMount(enabled);
      setLocalMount(status);
      push(t("messages.requestSuccess"), "success");
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    } finally {
      setMountBusy(false);
    }
  }

  useEffect(() => {
    void reloadAll();
  }, []);
  const activePublications = publications.filter((publication) => publication.state === "ready");

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
          <label className="cdb-trace-toggle local-mount-toggle">
            <input
              type="checkbox"
              checked={Boolean(localMount?.enabled)}
              disabled={mountBusy}
              onChange={(event) => void toggleLocalMount(event.target.checked)}
            />
            <span className="switch-track" aria-hidden="true">
              <span className="switch-thumb" />
            </span>
            <span className="switch-label">{t("targets.mountLocally")}</span>
          </label>
        </div>
        {localMount?.lastError ? <p className="notice notice-error">{localMount.lastError}</p> : null}
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
          <table className="table">
            <thead>
              <tr>
                <th>{t("targets.targetIqn")}</th>
                <th>{t("targets.deviceRole")}</th>
                <th>{t("targets.portal")}</th>
                <th className="connected-hosts-column">{t("targets.connectedHosts")}</th>
                <th>{t("common.state")}</th>
              </tr>
            </thead>
            <tbody>
              {activePublications.map((publication) => (
                <tr key={publication.publicationId}>
                  <td>{publication.targetIqn}</td>
                  <td>{publication.deviceRole}</td>
                  <td>{publication.portal || "-"}</td>
                  <td className="connected-hosts-column">{renderConnectedHosts(publication.connectedHosts, connectedHostsLabels)}</td>
                  <td><StatusBadge state={publication.state} /></td>
                </tr>
              ))}
              {activePublications.length === 0 ? (
                <tr>
                  <td colSpan={5}>{t("common.empty")}</td>
                </tr>
              ) : null}
            </tbody>
          </table>
        </div>
      </div>
    </section>
  );
}
