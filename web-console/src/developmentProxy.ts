export function createDevelopmentProxy(target: string) {
  return {
    "/v1": { target, changeOrigin: false },
    "/healthz": { target, changeOrigin: false },
  };
}
