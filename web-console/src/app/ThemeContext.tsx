import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { THEME_STORAGE_KEY } from "../utils/session";

type ThemeMode = "light" | "dark";

type ThemeContextValue = {
  theme: ThemeMode;
  setTheme: (next: ThemeMode) => void;
  toggleTheme: () => void;
};

const ThemeContext = createContext<ThemeContextValue | null>(null);

function detectSystemTheme(): ThemeMode {
  if (typeof window === "undefined") {
    return "light";
  }
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

function initialTheme(): ThemeMode {
  if (typeof window === "undefined") {
    return "light";
  }
  const localStore = window.localStorage as Storage | undefined;
  const saved =
    localStore && typeof localStore.getItem === "function"
      ? localStore.getItem(THEME_STORAGE_KEY)
      : null;
  if (saved === "light" || saved === "dark") {
    return saved;
  }
  return detectSystemTheme();
}

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [theme, setThemeState] = useState<ThemeMode>(() => initialTheme());

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    const localStore = window.localStorage as Storage | undefined;
    if (localStore && typeof localStore.setItem === "function") {
      localStore.setItem(THEME_STORAGE_KEY, theme);
    }
  }, [theme]);

  const value = useMemo<ThemeContextValue>(
    () => ({
      theme,
      setTheme: setThemeState,
      toggleTheme: () => setThemeState((prev) => (prev === "dark" ? "light" : "dark")),
    }),
    [theme]
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) {
    throw new Error("useTheme must be used within ThemeProvider");
  }
  return ctx;
}
