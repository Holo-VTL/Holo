/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly HOLO_DEV_BACKEND?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
