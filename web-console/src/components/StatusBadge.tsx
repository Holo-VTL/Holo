import { useTranslation } from "react-i18next";

type BadgeVariant = "ok" | "warn" | "error" | "muted";

function variantByState(input: string): BadgeVariant {
  const state = input.toLowerCase();
  if (state.includes("ready") || state.includes("healthy") || state.includes("ok") || state.includes("active")) {
    return "ok";
  }
  if (state.includes("degraded") || state.includes("warning") || state.includes("creating")) {
    return "warn";
  }
  if (state.includes("failed") || state.includes("down") || state.includes("disabled") || state.includes("error")) {
    return "error";
  }
  return "muted";
}

export function StatusBadge({ state }: { state: string }) {
  const { t } = useTranslation();
  const variant = variantByState(state);
  const normalized = state.toLowerCase();
  const map: Record<string, string> = {
    creating: t("targets.state.creating"),
    ready: t("targets.state.ready"),
    failed: t("targets.state.failed"),
    disabled: t("targets.state.disabled"),
    active: t("storage.active"),
    degraded: t("storage.degraded"),
  };

  return <span className={`status-badge status-${variant}`}>{map[normalized] || state || t("dashboard.unknown")}</span>;
}
