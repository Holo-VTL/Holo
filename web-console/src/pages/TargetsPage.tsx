import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { RefreshCw } from "lucide-react";
import { api } from "../services/api";
import { useToast } from "../components/Toast";
import { StatusBadge } from "../components/StatusBadge";
import type { LocalMountStatus, TargetPublication } from "../services/types";

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
                <th>{t("common.state")}</th>
              </tr>
            </thead>
            <tbody>
              {activePublications.map((publication) => (
                <tr key={publication.publicationId}>
                  <td>{publication.targetIqn}</td>
                  <td>{publication.deviceRole}</td>
                  <td>{publication.portal || "-"}</td>
                  <td><StatusBadge state={publication.state} /></td>
                </tr>
              ))}
              {activePublications.length === 0 ? (
                <tr>
                  <td colSpan={4}>{t("common.empty")}</td>
                </tr>
              ) : null}
            </tbody>
          </table>
        </div>
      </div>
    </section>
  );
}
