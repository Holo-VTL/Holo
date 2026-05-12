# Quickstart: Release Follow-Up Hardening

Run the local validation set from the repository root:

```bash
cd control-plane && go test ./...
cd control-plane && go vet ./...
cd ../data-plane && cargo test --locked
cargo clippy --locked --all-targets -- -D warnings
cd ..
bash scripts/lint-guardrails.sh
bash scripts/scan-rust-casts.sh
bash tests/integration/test_gated_scsi_e2e.sh
cd web-console && npm audit --audit-level=high
npm test
npm run build
```

For remote backend web-console development, keep the Vite server bound to loopback and set the backend explicitly:

```bash
cd web-console
HOLO_DEV_BACKEND=http://10.10.1.184 npm run dev:backend
```
