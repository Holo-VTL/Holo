import { FormEvent, useEffect, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Copy, Eye, EyeOff, KeyRound, Plus, RefreshCw, Trash2 } from "lucide-react";
import { api } from "../services/api";
import { AppModal } from "../components/AppModal";
import { useToast } from "../components/Toast";
import type { ISCSICredentialMetadata } from "../services/types";
import "../styles/security-page.css";

const chapAlphabet = Array.from({ length: 94 }, (_, index) => String.fromCharCode(33 + index)).join("");

export function generateChapSecret(excluded = ""): string {
  for (;;) {
    let output = "";
    while (output.length < 15) {
      const bytes = new Uint8Array(64);
      globalThis.crypto.getRandomValues(bytes);
      for (const byte of bytes) {
        if (byte < 188) output += chapAlphabet[byte % chapAlphabet.length];
        if (output.length === 15) break;
      }
    }
    if (!output.toUpperCase().startsWith("NULL") && output !== excluded) return output;
  }
}

function newManagementId(prefix: string, label: string): string {
  const slug = label
    .normalize("NFKD")
    .replace(/[\u0300-\u036f]/g, "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "")
    .slice(0, 36) || "item";
  const bytes = new Uint8Array(4);
  globalThis.crypto.getRandomValues(bytes);
  const suffix = Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  return `${prefix}-${slug}-${suffix}`;
}

type ChapSecretInputProps = {
  id: string;
  label: string;
  value: string;
  visible: boolean;
  required?: boolean;
  fullWidth?: boolean;
  hint?: string;
  actions?: ReactNode;
  onChange: (value: string) => void;
  onToggleVisibility: () => void;
  onCopy?: () => void;
};

function ChapSecretInput({ id, label, value, visible, required = false, fullWidth = false, hint, actions, onChange, onToggleVisibility, onCopy }: ChapSecretInputProps) {
  const { t } = useTranslation();
  return (
    <div className={fullWidth ? "security-field security-field-wide" : "security-field"}>
      <label htmlFor={id}>{label}</label>
      <div className="security-secret-control">
        <input id={id} className="input" type={visible ? "text" : "password"} autoComplete="new-password" maxLength={255} value={value} onChange={(event) => onChange(event.target.value)} required={required} />
        <button className="icon-btn security-secret-visibility" type="button" aria-label={visible ? t("iscsiSecurity.hideSecret") : t("iscsiSecurity.showSecret")} title={visible ? t("iscsiSecurity.hideSecret") : t("iscsiSecurity.showSecret")} onClick={onToggleVisibility}>
          {visible ? <EyeOff size={17} aria-hidden="true" /> : <Eye size={17} aria-hidden="true" />}
        </button>
        {actions || onCopy ? (
          <div className="security-secret-actions">
            {actions}
            {onCopy ? <button className="btn btn-quiet" type="button" disabled={!value} onClick={onCopy}><Copy size={15} aria-hidden="true" />{t("iscsiSecurity.copySecret")}</button> : null}
          </div>
        ) : null}
      </div>
      {hint ? <small>{hint}</small> : null}
    </div>
  );
}

async function copyTextToClipboard(value: string): Promise<boolean> {
  if (window.isSecureContext && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(value);
      return true;
    } catch {
      // Fall through to the copy command for local HTTP appliance consoles.
    }
  }
  const helper = document.createElement("textarea");
  helper.value = value;
  helper.setAttribute("readonly", "");
  helper.style.position = "fixed";
  helper.style.left = "-9999px";
  helper.style.opacity = "0";
  document.body.appendChild(helper);
  helper.select();
  const copied = typeof document.execCommand === "function" && document.execCommand("copy");
  helper.remove();
  return copied;
}

export function SecurityPage() {
  const { t } = useTranslation();
  const { push } = useToast();

  const [credentials, setCredentials] = useState<ISCSICredentialMetadata[]>([]);
  const [credentialForm, setCredentialForm] = useState({ credentialId: "", label: "", forwardUsername: "", forwardSecret: "", reverseUsername: "", reverseSecret: "" });
  const [securityBusy, setSecurityBusy] = useState(false);
  const [securityError, setSecurityError] = useState("");
  const [credentialFormOpen, setCredentialFormOpen] = useState(false);
  const [mutualChap, setMutualChap] = useState(false);
  const [forwardSecretVisible, setForwardSecretVisible] = useState(false);
  const [reverseSecretVisible, setReverseSecretVisible] = useState(false);

  useEffect(() => {
    void reloadSecurityMaterial().catch((err) => setSecurityError((err as Error).message || t("messages.requestFailed")));
  }, []);

  async function reloadSecurityMaterial() {
    const credentialRows = await api.iscsiSecurity.listCredentials();
    setCredentials(credentialRows);
  }

  function generateCredentialSecrets() {
    const forwardSecret = generateChapSecret();
    setCredentialForm((previous) => ({
      ...previous,
      forwardSecret,
      reverseSecret: mutualChap ? generateChapSecret(forwardSecret) : previous.reverseSecret,
    }));
    setForwardSecretVisible(true);
    if (mutualChap) setReverseSecretVisible(true);
  }

  async function copyChapSecret(secret: string) {
    if (!secret) return;
    if (await copyTextToClipboard(secret)) push(t("messages.copied"), "success");
    else push(t("iscsiSecurity.copySecretFailed"), "error");
  }

  function closeCredentialForm() {
    setCredentialFormOpen(false);
    setCredentialForm({ credentialId: "", label: "", forwardUsername: "", forwardSecret: "", reverseUsername: "", reverseSecret: "" });
    setMutualChap(false);
    setForwardSecretVisible(false);
    setReverseSecretVisible(false);
    setSecurityError("");
  }

  async function createCredential(event: FormEvent) {
    event.preventDefault();
    setSecurityBusy(true);
    setSecurityError("");
    try {
      const forwardUsername = credentialForm.forwardUsername.trim();
      const label = credentialForm.label.trim() || forwardUsername;
      const credentialId = credentialForm.credentialId.trim() || newManagementId("chap", label);
      await api.iscsiSecurity.createCredential({
        ...credentialForm,
        credentialId,
        label,
        forwardUsername,
        reverseUsername: mutualChap ? credentialForm.reverseUsername.trim() : "",
        reverseSecret: mutualChap ? credentialForm.reverseSecret : "",
        actor: "web-console",
      });
      setCredentialForm({ credentialId: "", label: "", forwardUsername: "", forwardSecret: "", reverseUsername: "", reverseSecret: "" });
      setMutualChap(false);
      setForwardSecretVisible(false);
      setReverseSecretVisible(false);
      setCredentialFormOpen(false);
      await reloadSecurityMaterial();
      push(t("messages.requestSuccess"), "success");
    } catch (err) {
      setSecurityError((err as Error).message || t("messages.requestFailed"));
    } finally {
      setSecurityBusy(false);
    }
  }

  async function deleteCredential(credentialId: string) {
    setSecurityBusy(true);
    setSecurityError("");
    try {
      await api.iscsiSecurity.deleteCredential(credentialId);
      await reloadSecurityMaterial();
    } catch (err) {
      setSecurityError((err as Error).message || t("messages.requestFailed"));
    } finally {
      setSecurityBusy(false);
    }
  }

  return (
    <section className="security-page">
      <header className="page-header security-page-header">
        <div>
          <h1 className="page-title">{t("security.pageHeading")}</h1>
          <p className="security-page-subtitle">{t("security.pageSummary")}</p>
        </div>
      </header>

      {securityError && !credentialFormOpen ? <p className="notice notice-error" role="alert">{securityError}</p> : null}

      <section className="security-section" aria-labelledby="chap-section-title">
        <div className="security-section-header">
          <div className="security-section-title-wrap">
            <KeyRound size={19} aria-hidden="true" />
            <div>
              <h2 id="chap-section-title">{t("iscsiSecurity.credentials")}</h2>
              <p>{t("iscsiSecurity.chapSectionHint")}</p>
            </div>
          </div>
          {!credentialFormOpen ? <button className="btn btn-primary" type="button" onClick={() => { setCredentialFormOpen(true); setSecurityError(""); }}><Plus size={16} aria-hidden="true" />{t("iscsiSecurity.addCredential")}</button> : null}
        </div>

        {credentialFormOpen ? (
          <AppModal open title={t("iscsiSecurity.newCredential")} description={t("iscsiSecurity.credentialIdHint")} busy={securityBusy} onClose={closeCredentialForm}>
            {securityError ? <p className="notice notice-error" role="alert">{securityError}</p> : null}
            <form className="security-form-panel app-dialog-form security-chap-credential-form" onSubmit={(event) => void createCredential(event)}>
            <div className="security-form-grid security-chap-credential-grid">
              <div className="security-field">
                <label htmlFor="chap-label">{t("iscsiSecurity.credentialLabel")}</label>
                <input id="chap-label" className="input" autoComplete="off" placeholder={t("iscsiSecurity.credentialLabelPlaceholder")} value={credentialForm.label} onChange={(event) => setCredentialForm((previous) => ({ ...previous, label: event.target.value }))} />
              </div>
              <div className="security-field">
                <label htmlFor="chap-target-user">{t("iscsiSecurity.forwardUsername")}</label>
                <input id="chap-target-user" className="input" autoComplete="off" value={credentialForm.forwardUsername} onChange={(event) => setCredentialForm((previous) => ({ ...previous, forwardUsername: event.target.value }))} required />
              </div>
              <ChapSecretInput
                id="chap-target-secret"
                label={t("iscsiSecurity.forwardSecret")}
                value={credentialForm.forwardSecret}
                visible={forwardSecretVisible}
                required
                fullWidth
                hint={t("iscsiSecurity.writeOnlyHint")}
                onChange={(value) => setCredentialForm((previous) => ({ ...previous, forwardSecret: value }))}
                onToggleVisibility={() => setForwardSecretVisible((value) => !value)}
                onCopy={() => void copyChapSecret(credentialForm.forwardSecret)}
                actions={<button className="btn btn-quiet" type="button" onClick={generateCredentialSecrets}><RefreshCw size={15} aria-hidden="true" />{t("iscsiSecurity.generateSecret")}</button>}
              />
            </div>

            <label className="cdb-trace-toggle security-mutual-toggle">
              <input type="checkbox" checked={mutualChap} onChange={(event) => setMutualChap(event.target.checked)} />
              <span className="switch-track" aria-hidden="true"><span className="switch-thumb" /></span>
              <span className="switch-label"><strong>{t("iscsiSecurity.enableMutualChap")}</strong><small>{t("iscsiSecurity.mutualChapHint")}</small></span>
            </label>

            {mutualChap ? (
              <div className="security-form-grid security-chap-credential-grid security-mutual-fields">
                <div className="security-field">
                  <label htmlFor="chap-initiator-user">{t("iscsiSecurity.reverseUsername")}</label>
                  <input id="chap-initiator-user" className="input" autoComplete="off" value={credentialForm.reverseUsername} onChange={(event) => setCredentialForm((previous) => ({ ...previous, reverseUsername: event.target.value }))} required />
                </div>
                <ChapSecretInput
                  id="chap-initiator-secret"
                  label={t("iscsiSecurity.reverseSecret")}
                  value={credentialForm.reverseSecret}
                  visible={reverseSecretVisible}
                  required
                  onChange={(value) => setCredentialForm((previous) => ({ ...previous, reverseSecret: value }))}
                  onToggleVisibility={() => setReverseSecretVisible((value) => !value)}
                />
              </div>
            ) : null}

            <div className="security-form-actions">
              <button className="btn btn-primary" type="submit" disabled={securityBusy}>{securityBusy ? t("common.loading") : t("iscsiSecurity.saveCredential")}</button>
              <button className="btn btn-quiet" type="button" disabled={securityBusy} onClick={closeCredentialForm}>{t("common.cancel")}</button>
            </div>
            </form>
          </AppModal>
        ) : null}

        {credentials.length ? (
          <div className="table-wrap">
            <table className="table security-table">
              <thead><tr><th>{t("iscsiSecurity.credentialLabelColumn")}</th><th>{t("iscsiSecurity.username")}</th><th>{t("iscsiSecurity.authenticationMode")}</th><th>{t("common.actions")}</th></tr></thead>
              <tbody>{credentials.map((credential) => <tr key={credential.credentialId}>
                <td>{credential.label === credential.username ? "—" : <strong>{credential.label}</strong>}</td>
                <td>{credential.username}</td>
                <td>{credential.mutualUsername ? t("iscsiSecurity.oneWayAndMutualChap") : t("iscsiSecurity.oneWayChap")}</td>
                <td><button className="btn btn-quiet" type="button" disabled={securityBusy} onClick={() => void deleteCredential(credential.credentialId)}><Trash2 size={15} aria-hidden="true" />{t("common.delete")}</button></td>
              </tr>)}</tbody>
            </table>
          </div>
        ) : !credentialFormOpen ? (
            <div className="security-empty-state">
              <KeyRound size={19} aria-hidden="true" />
              <div><strong>{t("iscsiSecurity.noCredentialsTitle")}</strong><p>{t("iscsiSecurity.noCredentialsHint")}</p></div>
            </div>
        ) : null}
      </section>

    </section>
  );
}
