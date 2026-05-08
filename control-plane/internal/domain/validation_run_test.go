package domain

import "testing"

func TestValidationRunCompletesPassedAndFailed(t *testing.T) {
	run, err := NewValidationRun("val-1", "pub-1")
	if err != nil {
		t.Fatalf("new validation run failed: %v", err)
	}
	if err := run.Complete(4096, 4096, "sha256:abc", "sha256:abc", "tests/evidence/val-1.json"); err != nil {
		t.Fatalf("complete passed run failed: %v", err)
	}
	if run.Status != ValidationPassed {
		t.Fatalf("expected passed status, got %s", run.Status)
	}

	run2, err := NewValidationRun("val-2", "pub-1")
	if err != nil {
		t.Fatalf("new validation run 2 failed: %v", err)
	}
	if err := run2.Complete(4096, 0, "sha256:abc", "sha256:def", "tests/evidence/val-2.json"); err != nil {
		t.Fatalf("complete failed run errored: %v", err)
	}
	if run2.Status != ValidationFailed {
		t.Fatalf("expected failed status, got %s", run2.Status)
	}
}

func TestValidationRunCompleteEmptyModePass(t *testing.T) {
	run, err := NewValidationRun("val-empty", "pub-1")
	if err != nil {
		t.Fatalf("new validation run failed: %v", err)
	}
	run.Mode = ValidationModeEmpty
	if err := run.Complete(0, 0, "sha256:e3b0c442", "sha256:e3b0c442", "tests/evidence/val-empty.json"); err != nil {
		t.Fatalf("complete empty-mode run failed: %v", err)
	}
	if run.Status != ValidationPassed {
		t.Fatalf("expected empty-mode run passed, got %s", run.Status)
	}
}
