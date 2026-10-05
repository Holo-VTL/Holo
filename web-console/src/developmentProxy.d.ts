export declare function createDevelopmentProxy(target: string): {
    "/v1": {
        target: string;
        changeOrigin: boolean;
    };
    "/healthz": {
        target: string;
        changeOrigin: boolean;
    };
};
