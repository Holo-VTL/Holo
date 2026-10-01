import { useTranslation } from "react-i18next";
import { SelectInput } from "./SelectInput";
import type { ISCSIAuthMode, ISCSICredentialMetadata } from "../services/types";

export type SecurityEditorScope = "create" | "library" | "drive" | "target";

export type SecurityEditorDraft = {
  authMode: ISCSIAuthMode | "inherit";
  credentialId: string;
  initiators: string;
};

type Props = {
  scope: SecurityEditorScope;
  draft: SecurityEditorDraft;
  credentials: ISCSICredentialMetadata[];
  loading: boolean;
  onChange: (draft: SecurityEditorDraft) => void;
  restrictInitiators?: boolean;
  onRestrictInitiatorsChange?: (restricted: boolean) => void;
  onRefresh?: () => void;
};

export function IscsiSecurityBlockEditor({ scope, draft, credentials, loading, onChange, restrictInitiators = false, onRestrictInitiatorsChange, onRefresh }: Props) {
  const { t } = useTranslation();
  const allowsInherit = scope === "drive" || scope === "target";
  const hasChap = draft.authMode === "chap" || draft.authMode === "mutual_chap";
  const eligibleCredentials = credentials.filter((item) => draft.authMode !== "mutual_chap" || item.mutualUsername);

  return (
    <>
      <div className={`resource-create-chap-pair form-row-wide${hasChap ? "" : " is-single"}`}>
        <div className="form-row">
          <label>{t("iscsiSecurity.authentication")}</label>
          <SelectInput
            value={draft.authMode}
            onChange={(value) => onChange({ ...draft, authMode: value as SecurityEditorDraft["authMode"], credentialId: "" })}
            options={[
              ...(allowsInherit ? [{ value: "inherit", label: t("iscsiSecurity.inherit") }] : []),
              { value: "none", label: t("iscsiSecurity.noChap") },
              { value: "chap", label: t("iscsiSecurity.oneWayChap") },
              { value: "mutual_chap", label: t("iscsiSecurity.mutualChap") },
            ]}
            ariaLabel={t("iscsiSecurity.authentication")}
          />
        </div>
        {hasChap ? (
          <div className="form-row">
            <label>{t("iscsiSecurity.credential")}</label>
            <SelectInput
              value={draft.credentialId}
              onChange={(value) => onChange({ ...draft, credentialId: value })}
              options={[
                { value: "", label: t("common.noSelection") },
                ...eligibleCredentials.map((item) => ({ value: item.credentialId, label: `${item.label} · ${item.username}` })),
              ]}
              ariaLabel={t("iscsiSecurity.credential")}
              required
              disabled={loading}
            />
            {!eligibleCredentials.length && !loading ? <small>{t("resources.createCredentialFirst")}</small> : null}
            {!eligibleCredentials.length && !loading ? (
              <div className="inline-actions">
                <a className="btn btn-quiet" href="/ui/security" target="_blank" rel="noreferrer">{t("resources.openConnectionSecurity")}</a>
                {onRefresh ? <button className="btn btn-quiet" type="button" disabled={loading} onClick={onRefresh}>{t("resources.refreshSecurityOptions")}</button> : null}
              </div>
            ) : null}
          </div>
        ) : null}
      </div>
      {hasChap || (scope !== "create" && draft.authMode === "none" && restrictInitiators) ? (
        <div className="form-row form-row-wide">
          <label htmlFor={`security-editor-initiators-${scope}`}>{t("iscsiSecurity.allowedInitiators")}</label>
          <textarea
            id={`security-editor-initiators-${scope}`}
            className="input"
            rows={3}
            value={draft.initiators}
            onChange={(event) => onChange({ ...draft, initiators: event.target.value })}
            aria-required={hasChap || restrictInitiators}
            placeholder="iqn.1991-05.com.microsoft:backup-host"
          />
          <small>{t("iscsiSecurity.initiatorHint")}</small>
        </div>
      ) : null}
      {scope !== "create" && draft.authMode === "none" ? (
        <label className="cdb-trace-toggle security-restriction-toggle form-row-wide">
          <input type="checkbox" checked={restrictInitiators} onChange={(event) => onRestrictInitiatorsChange?.(event.target.checked)} />
          <span className="switch-track" aria-hidden="true"><span className="switch-thumb" /></span>
          <span className="switch-label">{t("iscsiSecurity.restrictInitiators")}</span>
        </label>
      ) : null}
    </>
  );
}
