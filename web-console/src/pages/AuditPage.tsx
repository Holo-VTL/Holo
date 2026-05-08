import { FormEvent, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../services/api";
import { useToast } from "../components/Toast";
import type { AuditEvent, DiscoverableTarget, TargetPublication } from "../services/types";

export function AuditPage() {
  const { t } = useTranslation();
  const { push } = useToast();

  const [auditEvents, setAuditEvents] = useState<AuditEvent[]>([]);
  const [visibleRows, setVisibleRows] = useState<TargetPublication[]>([]);
  const [discoveryRows, setDiscoveryRows] = useState<DiscoverableTarget[]>([]);

  const [visibleForm, setVisibleForm] = useState({ initiator: "", actor: "web-console" });
  const [discoveryForm, setDiscoveryForm] = useState({ initiator: "", actor: "web-console", portal: "" });

  useEffect(() => {
    void loadAudit();
  }, []);

  async function loadAudit() {
    try {
      const rows = await api.ops.auditEvents();
      setAuditEvents(rows);
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    }
  }

  async function queryVisible(event: FormEvent) {
    event.preventDefault();
    try {
      const rows = await api.targets.visible(visibleForm);
      setVisibleRows(rows);
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    }
  }

  async function queryDiscovery(event: FormEvent) {
    event.preventDefault();
    try {
      const rows = await api.targets.discovery(discoveryForm);
      setDiscoveryRows(rows);
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    }
  }

  return (
    <section>
      <div className="page-header">
        <h1 className="page-title">{t("audit.title")}</h1>
      </div>

      <div className="grid-2">
        <div className="panel">
          <h3>{t("audit.targetVisible")}</h3>
          <form className="form-grid" onSubmit={queryVisible}>
            <div className="form-row"><label>{t("audit.initiator")}</label><input className="input" value={visibleForm.initiator} onChange={(e) => setVisibleForm((p) => ({ ...p, initiator: e.target.value }))} required /></div>
            <div className="inline-actions"><button className="btn btn-primary" type="submit">{t("common.run")}</button></div>
          </form>
          <div className="table-wrap" style={{ marginTop: 10 }}>
            <table className="table">
              <thead><tr><th>{t("targets.publicationId")}</th><th>{t("targets.targetIqn")}</th><th>{t("targets.portal")}</th><th>{t("common.state")}</th></tr></thead>
              <tbody>{visibleRows.map((row) => <tr key={row.publicationId}><td>{row.publicationId}</td><td>{row.targetIqn}</td><td>{row.portal}</td><td>{row.state}</td></tr>)}</tbody>
            </table>
          </div>
        </div>

        <div className="panel">
          <h3>{t("audit.targetDiscovery")}</h3>
          <form className="form-grid" onSubmit={queryDiscovery}>
            <div className="form-row"><label>{t("audit.initiator")}</label><input className="input" value={discoveryForm.initiator} onChange={(e) => setDiscoveryForm((p) => ({ ...p, initiator: e.target.value }))} required /></div>
            <div className="form-row"><label>{t("audit.portal")}</label><input className="input" value={discoveryForm.portal} onChange={(e) => setDiscoveryForm((p) => ({ ...p, portal: e.target.value }))} /></div>
            <div className="inline-actions"><button className="btn btn-primary" type="submit">{t("common.run")}</button></div>
          </form>
          <div className="table-wrap" style={{ marginTop: 10 }}>
            <table className="table">
              <thead><tr><th>{t("targets.publicationId")}</th><th>{t("targets.targetIqn")}</th><th>{t("targets.portal")}</th><th>{t("common.state")}</th></tr></thead>
              <tbody>{discoveryRows.map((row) => <tr key={row.publicationId}><td>{row.publicationId}</td><td>{row.targetIqn}</td><td>{row.portal}</td><td>{row.state}</td></tr>)}</tbody>
            </table>
          </div>
        </div>
      </div>

      <div className="panel" style={{ marginTop: 12 }}>
        <h3>{t("audit.auditEvents")}</h3>
        <div className="inline-actions" style={{ marginBottom: 10 }}>
          <button className="btn" onClick={() => void loadAudit()}>{t("common.refresh")}</button>
        </div>
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>{t("audit.action")}</th>
                <th>{t("common.actor")}</th>
                <th>{t("audit.objectType")}</th>
                <th>{t("audit.objectId")}</th>
                <th>{t("common.result")}</th>
                <th>{t("audit.occurredAt")}</th>
              </tr>
            </thead>
            <tbody>
              {auditEvents.map((event) => (
                <tr key={event.eventId}>
                  <td>{event.action}</td>
                  <td>{event.actor}</td>
                  <td>{event.objectType}</td>
                  <td>{event.objectId}</td>
                  <td>{event.result}</td>
                  <td>{new Date(event.occurredAt).toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    </section>
  );
}
