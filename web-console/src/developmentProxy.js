export function createDevelopmentProxy(target) {
    return {
        "/v1": { target: target, changeOrigin: false },
        "/healthz": { target: target, changeOrigin: false },
    };
}
