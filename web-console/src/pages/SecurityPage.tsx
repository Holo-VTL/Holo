import { FormEvent, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../services/api";
import { useToast } from "../components/Toast";
import { SelectInput } from "../components/SelectInput";
import { parseRulesJson, toLocalDatetimeValue } from "../utils/format";
import type { InitiatorRule, TargetPublication, VirtualCartridge } from "../services/types";

const defaultRules = JSON.stringify(
  [
    {
      ruleId: "rule-allow-default",
      initiator: "iqn.1991-05.com.microsoft:demo-initiator",
      permission: "allow",
      priority: 10,
    },
  ],
  null,
  2
);

export function SecurityPage() {
  const { t } = useTranslation();
  const { push } = useToast();

  const [publications, setPublications] = useState<TargetPublication[]>([]);
  const [cartridges, setCartridges] = useState<VirtualCartridge[]>([]);
  const [rules, setRules] = useState<InitiatorRule[]>([]);

  const [accessPolicyForm, setAccessPolicyForm] = useState({
    policyId: "",
    scope: "global" as "global" | "library" | "drive",
    subject: "",
    permission: "allow" as "allow" | "deny",
    effectiveFrom: new Date().toISOString(),
    effectiveTo: "",
  });

  const [retentionForm, setRetentionForm] = useState({
    retentionId: "",
    cartridgeId: "",
    mode: "worm" as "worm" | "governance",
    lockUntil: toLocalDatetimeValue(new Date(Date.now() + 24 * 3600 * 1000)),
    createdBy: "web-console",
  });

  const [ruleForm, setRuleForm] = useState({
    publicationId: "",
    actor: "web-console",
    initiator: "iqn.1991-05.com.microsoft:demo-initiator",
    rulesJson: defaultRules,
  });

  useEffect(() => {
    void reloadRefs();
  }, []);

  async function reloadRefs() {
    const [pubRows, cartridgeRows] = await Promise.all([
      api.targets.listPublications(),
      api.resources.listCartridges(),
    ]);
    setPublications(pubRows);
    setCartridges(cartridgeRows);
  }

  async function createAccessPolicy(event: FormEvent) {
    event.preventDefault();
    try {
      await api.policy.createAccessPolicy({
        ...accessPolicyForm,
        effectiveTo: accessPolicyForm.effectiveTo || undefined,
      });
      push(t("messages.requestSuccess"), "success");
      setAccessPolicyForm((prev) => ({ ...prev, policyId: "", subject: "" }));
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    }
  }

  async function createRetentionPolicy(event: FormEvent) {
    event.preventDefault();
    try {
      await api.policy.createRetentionPolicy({
        ...retentionForm,
        lockUntil: new Date(retentionForm.lockUntil).toISOString(),
      });
      push(t("messages.requestSuccess"), "success");
      setRetentionForm((prev) => ({ ...prev, retentionId: "" }));
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    }
  }

  async function replaceRules(event: FormEvent) {
    event.preventDefault();
    if (!ruleForm.publicationId) {
      return;
    }

    let parsed: unknown;
    try {
      parsed = parseRulesJson(ruleForm.rulesJson);
    } catch {
      push(t("messages.invalidJson"), "error");
      return;
    }

    try {
      await api.targets.replaceAccessRules(ruleForm.publicationId, {
        actor: ruleForm.actor,
        rules: parsed as Array<Record<string, unknown>>,
      });
      push(t("messages.requestSuccess"), "success");
      const list = await api.targets.listAccessRules(ruleForm.publicationId);
      setRules(list);
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    }
  }

  async function loadRules() {
    if (!ruleForm.publicationId) {
      return;
    }
    try {
      const list = await api.targets.listAccessRules(ruleForm.publicationId);
      setRules(list);
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    }
  }

  async function authorize() {
    if (!ruleForm.publicationId || !ruleForm.initiator) {
      return;
    }
    try {
      const decision = await api.targets.authorize(ruleForm.publicationId, {
        initiator: ruleForm.initiator,
        actor: ruleForm.actor,
      });
      push(`${t("security.authorize")}: ${decision.decision}`, "info");
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    }
  }

  async function rollbackRules() {
    if (!ruleForm.publicationId) {
      return;
    }
    try {
      await api.targets.rollbackAccess(ruleForm.publicationId, { actor: ruleForm.actor });
      push(t("messages.requestSuccess"), "success");
      await loadRules();
    } catch (err) {
      push((err as Error).message || t("messages.requestFailed"), "error");
    }
  }

  return (
    <section>
      <div className="page-header">
        <h1 className="page-title">{t("security.title")}</h1>
      </div>

      <div className="grid-2">
        <div className="panel">
          <h3>{t("security.createAccessPolicy")}</h3>
          <form className="form-grid" onSubmit={createAccessPolicy}>
            <div className="form-row"><label>{t("security.policyId")}</label><input className="input" value={accessPolicyForm.policyId} onChange={(e) => setAccessPolicyForm((p) => ({ ...p, policyId: e.target.value }))} required /></div>
            <div className="form-row"><label>{t("security.scope")}</label><SelectInput value={accessPolicyForm.scope} onChange={(value) => setAccessPolicyForm((p) => ({ ...p, scope: value as "global" | "library" | "drive" }))} options={[{ value: "global", label: "global" }, { value: "library", label: "library" }, { value: "drive", label: "drive" }]} ariaLabel={t("security.scope")} /></div>
            <div className="form-row"><label>{t("security.subject")}</label><input className="input" value={accessPolicyForm.subject} onChange={(e) => setAccessPolicyForm((p) => ({ ...p, subject: e.target.value }))} required /></div>
            <div className="form-row"><label>{t("security.permission")}</label><SelectInput value={accessPolicyForm.permission} onChange={(value) => setAccessPolicyForm((p) => ({ ...p, permission: value as "allow" | "deny" }))} options={[{ value: "allow", label: "allow" }, { value: "deny", label: "deny" }]} ariaLabel={t("security.permission")} /></div>
            <div className="form-row"><label>{t("security.effectiveFrom")}</label><input className="input" type="datetime-local" value={toLocalDatetimeValue(new Date(accessPolicyForm.effectiveFrom))} onChange={(e) => setAccessPolicyForm((p) => ({ ...p, effectiveFrom: new Date(e.target.value).toISOString() }))} /></div>
            <div className="form-row"><label>{t("security.effectiveTo")}</label><input className="input" type="datetime-local" value={accessPolicyForm.effectiveTo} onChange={(e) => setAccessPolicyForm((p) => ({ ...p, effectiveTo: e.target.value }))} /></div>
            <div className="inline-actions"><button className="btn btn-primary" type="submit">{t("common.create")}</button></div>
          </form>
        </div>

        <div className="panel">
          <h3>{t("security.createRetentionPolicy")}</h3>
          <form className="form-grid" onSubmit={createRetentionPolicy}>
            <div className="form-row"><label>{t("security.retentionId")}</label><input className="input" value={retentionForm.retentionId} onChange={(e) => setRetentionForm((p) => ({ ...p, retentionId: e.target.value }))} required /></div>
            <div className="form-row"><label>{t("security.cartridgeId")}</label><SelectInput value={retentionForm.cartridgeId} onChange={(value) => setRetentionForm((p) => ({ ...p, cartridgeId: value }))} options={[{ value: "", label: t("common.noSelection") }, ...cartridges.map((cartridge) => ({ value: cartridge.cartridgeId, label: cartridge.cartridgeId }))]} ariaLabel={t("security.cartridgeId")} required /></div>
            <div className="form-row"><label>{t("security.mode")}</label><SelectInput value={retentionForm.mode} onChange={(value) => setRetentionForm((p) => ({ ...p, mode: value as "worm" | "governance" }))} options={[{ value: "worm", label: "worm" }, { value: "governance", label: "governance" }]} ariaLabel={t("security.mode")} /></div>
            <div className="form-row"><label>{t("security.lockUntil")}</label><input className="input" type="datetime-local" value={retentionForm.lockUntil} onChange={(e) => setRetentionForm((p) => ({ ...p, lockUntil: e.target.value }))} required /></div>
            <div className="inline-actions"><button className="btn btn-primary" type="submit">{t("common.create")}</button></div>
          </form>
        </div>
      </div>

      <div className="panel" style={{ marginTop: 12 }}>
        <h3>{t("security.publicationAccess")}</h3>
        <form className="form-grid" onSubmit={replaceRules}>
          <div className="form-row"><label>{t("security.publicationId")}</label><SelectInput value={ruleForm.publicationId} onChange={(value) => setRuleForm((p) => ({ ...p, publicationId: value }))} options={[{ value: "", label: t("common.noSelection") }, ...publications.map((item) => ({ value: item.publicationId, label: item.publicationId }))]} ariaLabel={t("security.publicationId")} /></div>
          <div className="form-row"><label>{t("security.initiator")}</label><input className="input" value={ruleForm.initiator} onChange={(e) => setRuleForm((p) => ({ ...p, initiator: e.target.value }))} /></div>
          <div className="form-row"><label>{t("security.rulesJson")}</label><textarea className="input" value={ruleForm.rulesJson} onChange={(e) => setRuleForm((p) => ({ ...p, rulesJson: e.target.value }))} /></div>
          <div className="inline-actions">
            <button className="btn btn-primary" type="submit">{t("security.replaceRules")}</button>
            <button className="btn" type="button" onClick={() => void loadRules()}>{t("common.refresh")}</button>
            <button className="btn" type="button" onClick={() => void authorize()}>{t("security.authorize")}</button>
            <button className="btn" type="button" onClick={() => void rollbackRules()}>{t("security.rollbackRules")}</button>
          </div>
        </form>

        <div className="table-wrap" style={{ marginTop: 10 }}>
          <table className="table">
            <thead><tr><th>ruleId</th><th>{t("security.initiator")}</th><th>{t("security.permission")}</th><th>priority</th></tr></thead>
            <tbody>{rules.map((rule) => <tr key={rule.ruleId}><td>{rule.ruleId}</td><td>{rule.initiator}</td><td>{rule.permission}</td><td>{rule.priority}</td></tr>)}</tbody>
          </table>
        </div>
      </div>
    </section>
  );
}
