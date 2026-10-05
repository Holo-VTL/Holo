import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { loadEnv } from "vite";
import { createDevelopmentProxy } from "./src/developmentProxy";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, ".", "");
  const devBackend = env.HOLO_DEV_BACKEND || "http://127.0.0.1";

  return {
    plugins: [react()],
    base: mode === "development" ? "/" : "/ui/",
    server: {
      host: "127.0.0.1",
      port: 5173,
      proxy: createDevelopmentProxy(devBackend),
    },
    test: {
      environment: "jsdom",
      setupFiles: ["./src/test/setup.ts"],
    },
  };
});
