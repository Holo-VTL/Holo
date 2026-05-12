/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly HOLO_DEV_BACKEND?: string;
  readonly BASE_URL: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
