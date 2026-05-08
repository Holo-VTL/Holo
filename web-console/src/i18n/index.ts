import i18n from "i18next";
import { initReactI18next } from "react-i18next";
import zhCN from "./locales/zh-CN.json";
import enUS from "./locales/en-US.json";
import { LOCALE_MODE_STORAGE_KEY } from "../utils/session";

export const supportedLocales = ["zh-CN", "en-US"] as const;
export type LocaleCode = (typeof supportedLocales)[number];
export type LocaleMode = "auto" | LocaleCode;

export function resolveNavigatorLocale(): LocaleCode {
  if (typeof window === "undefined") {
    return "zh-CN";
  }
  const lang = window.navigator.language.toLowerCase();
  if (lang.startsWith("zh")) {
    return "zh-CN";
  }
  return "en-US";
}

function resolveSavedLocaleMode(): LocaleMode {
  if (typeof window === "undefined") {
    return "auto";
  }
  const localStore = window.localStorage as Storage | undefined;
  const saved =
    localStore && typeof localStore.getItem === "function"
      ? localStore.getItem(LOCALE_MODE_STORAGE_KEY)
      : null;
  if (saved === "zh-CN" || saved === "en-US" || saved === "auto") {
    return saved;
  }
  return "auto";
}

function resolveInitialLocale(): LocaleCode {
  const mode = resolveSavedLocaleMode();
  if (mode === "auto") {
    return resolveNavigatorLocale();
  }
  return mode;
}

void i18n.use(initReactI18next).init({
  resources: {
    "zh-CN": { translation: zhCN },
    "en-US": { translation: enUS },
  },
  lng: resolveInitialLocale(),
  fallbackLng: "en-US",
  interpolation: {
    escapeValue: false,
  },
});

export default i18n;
