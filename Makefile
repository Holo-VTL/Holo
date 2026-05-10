.PHONY: help e2e-scsi

help:
	@echo "holo bootstrap workspace"
	@echo "  e2e-scsi  Run gated privileged Linux SCSI E2E smoke"

e2e-scsi:
	@test -n "$${HOLO_E2E_IQN:-}" || { echo "HOLO_E2E_IQN is required" >&2; exit 2; }
	HOLO_RUN_PRIVILEGED_SCSI_E2E=1 bash scripts/gated-scsi-e2e.sh \
		--portal "$${HOLO_E2E_PORTAL:-127.0.0.1:3260}" \
		--iqn "$${HOLO_E2E_IQN}" \
		--write-read-smoke
