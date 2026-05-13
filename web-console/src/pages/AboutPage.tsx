import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Code2, Download, FileText, Info, Server, ShieldCheck } from "lucide-react";
import { api } from "../services/api";
import type { CDBTraceStatus, SystemOverview } from "../services/types";

function formatUptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) {
    return "0m";
  }
  const totalMinutes = Math.floor(seconds / 60);
  const days = Math.floor(totalMinutes / (24 * 60));
  const hours = Math.floor((totalMinutes % (24 * 60)) / 60);
  const minutes = totalMinutes % 60;
  if (days > 0) {
    return `${days}d ${hours}h`;
  }
  if (hours > 0) {
    return `${hours}h ${minutes}m`;
  }
  return `${minutes}m`;
}

export function AboutPage() {
  const { t } = useTranslation();
  const [overview, setOverview] = useState<SystemOverview | null>(null);
  const [loadError, setLoadError] = useState("");
  const [supportDownloading, setSupportDownloading] = useState(false);
  const [supportError, setSupportError] = useState("");
  const [cdbTrace, setCDBTrace] = useState<CDBTraceStatus | null>(null);
  const [cdbTraceUpdating, setCDBTraceUpdating] = useState(false);

  const version = import.meta.env.VITE_APP_VERSION || "v0.0.0";
  const buildMode = import.meta.env.MODE || "production";
  const buildLabel = t("common.buildLabel", { version: version.startsWith('v') ? version.toUpperCase() : `V${version.toUpperCase()}` });

  useEffect(() => {
    let cancelled = false;
    (async () => {
      setLoadError("");
      try {
        const [overviewResult, cdbTraceResult] = await Promise.allSettled([
          api.ops.systemOverview(),
          api.ops.cdbTrace(),
        ]);
        if (!cancelled) {
          if (overviewResult.status === "fulfilled") {
            setOverview(overviewResult.value);
          } else {
            setLoadError((overviewResult.reason as Error)?.message || t("messages.apiError"));
          }
          setCDBTrace(cdbTraceResult.status === "fulfilled" ? cdbTraceResult.value : null);
        }
      } catch (err) {
        if (!cancelled) {
          setLoadError((err as Error).message || t("messages.apiError"));
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [t]);

  async function downloadSupportBundle() {
    setSupportDownloading(true);
    setSupportError("");
    try {
      const { blob, filename } = await api.ops.supportBundle();
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = filename;
      document.body.appendChild(anchor);
      anchor.click();
      anchor.remove();
      URL.revokeObjectURL(url);
    } catch (err) {
      setSupportError((err as Error).message || t("dashboard.supportBundleError"));
    } finally {
      setSupportDownloading(false);
    }
  }

  async function toggleCDBTrace(enabled: boolean) {
    setSupportError("");
    setCDBTraceUpdating(true);
    try {
      setCDBTrace(await api.ops.setCDBTrace(enabled));
    } catch (err) {
      setSupportError((err as Error).message || t("dashboard.cdbTraceUpdateError"));
    } finally {
      setCDBTraceUpdating(false);
    }
  }

  return (
    <section className="about-page">
      <div className="page-header">
        <div>
          <p className="eyebrow">{buildLabel}</p>
          <h1>{t("about.title")}</h1>
        </div>
      </div>

      <div className="about-hero panel">
        <div className="about-hero-mark">
          <Info size={28} />
        </div>
        <div>
          <h2>{t("app.title")}</h2>
          <p>{t("about.productSummary")}</p>
        </div>
      </div>

      {supportError ? <p className="notice notice-error">{supportError}</p> : null}
      {loadError ? <p className="notice">{loadError}</p> : null}

      <div className="about-grid">
        <section className="panel about-card about-support-card">
          <div className="about-card-title">
            <Download size={18} />
            <h3>{t("dashboard.supportBundleTitle")}</h3>
          </div>
          <p className="about-support-copy">{t("dashboard.supportBundleHint")}</p>
          <div className="about-support-actions">
            <label className="cdb-trace-toggle">
              <input
                type="checkbox"
                checked={Boolean(cdbTrace?.enabled)}
                disabled={cdbTraceUpdating || !cdbTrace}
                onChange={(event) => void toggleCDBTrace(event.currentTarget.checked)}
              />
              <span className="switch-track" aria-hidden="true">
                <span className="switch-thumb" />
              </span>
              <span className="switch-label">{t("dashboard.cdbTraceToggle")}</span>
            </label>
            <button className="btn btn-primary" type="button" onClick={() => void downloadSupportBundle()} disabled={supportDownloading}>
              <Download size={14} />
              {supportDownloading ? t("dashboard.supportBundlePreparing") : t("dashboard.supportBundleDownload")}
            </button>
          </div>
        </section>

        <section className="panel about-card">
          <div className="about-card-title">
            <Code2 size={18} />
            <h3>{t("about.versionTitle")}</h3>
          </div>
          <dl className="about-meta-list">
            <div>
              <dt>{t("about.consoleVersion")}</dt>
              <dd>{version}</dd>
            </div>
            <div>
              <dt>{t("about.buildMode")}</dt>
              <dd>{buildMode}</dd>
            </div>
            <div>
              <dt>{t("about.releaseChannel")}</dt>
              <dd>{(version !== "v0.0.0" && !version.includes("dev") && !version.includes("beta") && !version.includes("preview")) ? t("about.channelStable") : t("about.channelDevelopment")}</dd>
            </div>
          </dl>
        </section>

        <section className="panel about-card">
          <div className="about-card-title">
            <Server size={18} />
            <h3>{t("about.systemTitle")}</h3>
          </div>
          <dl className="about-meta-list">
            <div>
              <dt>{t("dashboard.hostname")}</dt>
              <dd>{overview?.hostname || "-"}</dd>
            </div>
            <div>
              <dt>{t("dashboard.uptime")}</dt>
              <dd>{overview ? formatUptime(overview.uptimeSeconds) : "-"}</dd>
            </div>
            <div>
              <dt>{t("about.collectedAt")}</dt>
              <dd>{overview?.collectedAt ? new Date(overview.collectedAt).toLocaleString() : "-"}</dd>
            </div>
          </dl>
        </section>

        <section className="panel about-card">
          <div className="about-card-title">
            <ShieldCheck size={18} />
            <h3>{t("about.maintainerTitle")}</h3>
          </div>
          <dl className="about-meta-list">
            <div>
              <dt>{t("about.author")}</dt>
              <dd>Lei Wei made with ❤️</dd>
            </div>
            <div>
              <dt>{t("about.copyright")}</dt>
              <dd>© 2026 Lei Wei</dd>
            </div>
          </dl>
        </section>

        <section className="panel about-card">
          <div className="about-card-title">
            <FileText size={18} />
            <h3>{t("about.licenseTitle")}</h3>
          </div>
          <dl className="about-meta-list">
            <div>
              <dt>{t("about.license")}</dt>
              <dd>MIT License</dd>
            </div>
            <div>
              <dt>{t("about.notice")}</dt>
              <dd>{t("about.noticeText")}</dd>
            </div>
          </dl>
        </section>
      </div>
    </section>
  );
}
