import { useMemo, useState } from "react";
import { NavLink, Outlet, useLocation } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { useTheme } from "../app/ThemeContext";
import { resolveNavigatorLocale, type LocaleMode } from "../i18n";
import { LOCALE_MODE_STORAGE_KEY } from "../utils/session";
import {
  ActivitySquare,
  ChevronLeft,
  ChevronRight,
  DatabaseBackup,
  Globe,
  HardDrive,
  Info,
  Languages,
  LetterText,
  LibraryBig,
  Moon,
  Sun,
} from "lucide-react";

type NavItem = {
  to: string;
  labelKey: string;
  icon: typeof ActivitySquare;
};

export function AppShell() {
  const { t, i18n } = useTranslation();
  const { theme, toggleTheme } = useTheme();
  const location = useLocation();
  const [sidebarExpanded, setSidebarExpanded] = useState(true);
  const [localeMode, setLocaleMode] = useState<LocaleMode>(() => {
    if (typeof window === "undefined") {
      return "auto";
    }
    const localStore = window.localStorage as Storage | undefined;
    const saved =
      localStore && typeof localStore.getItem === "function"
        ? localStore.getItem(LOCALE_MODE_STORAGE_KEY)
        : null;
    if (saved === "auto" || saved === "zh-CN" || saved === "en-US") {
      return saved;
    }
    return "auto";
  });

  const items: NavItem[] = [
    { to: "/", labelKey: "nav.dashboard", icon: ActivitySquare },
    { to: "/storage", labelKey: "nav.storage", icon: HardDrive },
    { to: "/resources", labelKey: "nav.resources", icon: LibraryBig },
    { to: "/targets", labelKey: "nav.targets", icon: DatabaseBackup },
    { to: "/about", labelKey: "nav.about", icon: Info },
  ];

  const currentPage = useMemo(() => {
    const active = items.find((item) => (item.to === "/" ? location.pathname === "/" : location.pathname.startsWith(item.to)));
    return active ? t(active.labelKey) : t("nav.dashboard");
  }, [items, location.pathname, t]);

  function nextLocaleMode(mode: LocaleMode): LocaleMode {
    if (mode === "auto") {
      return "en-US";
    }
    if (mode === "en-US") {
      return "zh-CN";
    }
    return "auto";
  }

  async function cycleLocaleMode() {
    const next = nextLocaleMode(localeMode);
    setLocaleMode(next);
    if (typeof window !== "undefined") {
      const localStore = window.localStorage as Storage | undefined;
      if (localStore && typeof localStore.setItem === "function") {
        localStore.setItem(LOCALE_MODE_STORAGE_KEY, next);
      }
    }
    const resolved = next === "auto" ? resolveNavigatorLocale() : next;
    await i18n.changeLanguage(resolved);
  }

  const localeButtonTitle =
    localeMode === "auto"
      ? t("common.localeAuto")
      : localeMode === "en-US"
        ? t("locale.enUS")
        : t("locale.zhCN");
  const LocaleModeIcon = localeMode === "auto" ? Globe : localeMode === "en-US" ? LetterText : Languages;
  const productVersionStatus = import.meta.env.VITE_APP_VERSION || "v0.0.0";
  const buildLabel = t("common.buildLabel", { version: productVersionStatus.startsWith('v') ? productVersionStatus.toUpperCase() : `V${productVersionStatus.toUpperCase()}` });

  return (
    <div className={sidebarExpanded ? "layout-root" : "layout-root layout-sidebar-collapsed"}>
      <aside className="layout-sidebar">
        <div className="sidebar-head">
          <div className="brand-box">
            <div className="brand-mark">
              <DatabaseBackup size={18} />
            </div>
            <div className="brand-meta">
              <div className="brand-title">{t("app.title")}</div>
            </div>
          </div>
        </div>

        <nav className="nav-stack">
          {items.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.to === "/"}
              className={({ isActive }) => (isActive ? "nav-link nav-link-active" : "nav-link")}
            >
              <item.icon size={16} />
              <span className="nav-label">{t(item.labelKey)}</span>
            </NavLink>
          ))}
        </nav>
        <button
          className="icon-btn sidebar-toggle"
          type="button"
          onClick={() => setSidebarExpanded((prev) => !prev)}
          aria-label={t("common.toggleSidebar")}
          title={t("common.toggleSidebar")}
        >
          {sidebarExpanded ? <ChevronLeft size={17} /> : <ChevronRight size={17} />}
        </button>
      </aside>

      <div className="layout-main">
        <header className="topbar">
          <div className="topbar-left">
            <h2 className="topbar-title">{currentPage}</h2>
          </div>

          <div className="topbar-actions">
            <NavLink className="version-pill version-link" to="/about" title={t("about.title")}>
              {buildLabel}
            </NavLink>
            <button className="icon-btn" onClick={toggleTheme} aria-label={theme === "dark" ? t("theme.light") : t("theme.dark")}>
              {theme === "dark" ? <Sun size={16} /> : <Moon size={16} />}
            </button>

            <button
              className="icon-btn locale-toggle-btn"
              onClick={() => void cycleLocaleMode()}
              title={localeButtonTitle}
              aria-label={localeButtonTitle}
            >
              <LocaleModeIcon size={16} />
            </button>

          </div>
        </header>

        <main className="page-container">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
