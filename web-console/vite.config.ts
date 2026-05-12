import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { loadEnv } from "vite";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, ".", "");
  const devBackend = env.HOLO_DEV_BACKEND || "http://127.0.0.1";

  return {
    plugins: [react()],
    base: mode === "development" ? "/" : "/ui/",
    server: {
      host: "127.0.0.1",
      port: 5173,
      proxy: {
        "/v1": {
          target: devBackend,
          changeOrigin: true,
        },
        "/healthz": {
          target: devBackend,
          changeOrigin: true,
        },
      },
    },
    test: {
      environment: "jsdom",
      setupFiles: ["./src/test/setup.ts"],
    },
  };
});
