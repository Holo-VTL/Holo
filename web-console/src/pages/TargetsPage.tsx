import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { RefreshCw } from "lucide-react";
import { api } from "../services/api";
import { StatusBadge } from "../components/StatusBadge";
import type { TargetPublication } from "../services/types";

export function TargetsPage() {
  const { t } = useTranslation();
  const [publications, setPublications] = useState<TargetPublication[]>([]);
  const [error, setError] = useState("");

  async function reloadAll() {
    setError("");
    try {
      const pubRows = await api.targets.listPublications();
      setPublications(pubRows);
    } catch (err) {
      setError((err as Error).message || t("messages.apiError"));
    }
  }

  useEffect(() => {
    void reloadAll();
  }, []);
  const activePublications = publications.filter((publication) => publication.state === "ready");

  return (
    <section>
      <div className="page-header">
        <h1 className="page-title">{t("targets.title")}</h1>
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
