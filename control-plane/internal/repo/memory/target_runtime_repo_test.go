package memory

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestTargetRuntimeRepoCapsValidationMediaEntries(t *testing.T) {
	repo := NewTargetRuntimeRepo()
	ctx := context.Background()

	for i := 0; i < maxValidationMediaEntries+5; i++ {
		pubID := fmt.Sprintf("pub-%04d", i)
		pub, err := domain.NewTargetPublication(pubID, "pool", "lib", "drive", "cart", "iqn.2026-04.ai.holo:test")
		if err != nil {
			t.Fatalf("new publication failed: %v", err)
		}
		if err := repo.SavePublication(ctx, pub); err != nil {
			t.Fatalf("save publication failed: %v", err)
		}
		if err := repo.WriteValidationMedia(ctx, pubID, []byte{byte(i % 255), 0xAA}); err != nil {
			t.Fatalf("write validation media failed: %v", err)
		}
	}

	oldestPayload, err := repo.ReadValidationMedia(ctx, "pub-0000")
	if err != nil {
		t.Fatalf("read oldest validation media failed: %v", err)
	}
	if len(oldestPayload) != 0 {
		t.Fatalf("expected oldest payload to be evicted, got len=%d", len(oldestPayload))
	}

	latestPayload, err := repo.ReadValidationMedia(ctx, fmt.Sprintf("pub-%04d", maxValidationMediaEntries+4))
	if err != nil {
		t.Fatalf("read latest validation media failed: %v", err)
	}
	if len(latestPayload) == 0 {
		t.Fatal("expected latest payload to be retained")
	}
}

func TestTargetRuntimeRepoEvictOldValidationMedia(t *testing.T) {
	repo := NewTargetRuntimeRepo()
	ctx := context.Background()
	pub, err := domain.NewTargetPublication("pub-aging", "pool", "lib", "drive", "cart", "iqn.2026-04.ai.holo:aging")
	if err != nil {
		t.Fatalf("new publication failed: %v", err)
	}
	if err := repo.SavePublication(ctx, pub); err != nil {
		t.Fatalf("save publication failed: %v", err)
	}
	if err := repo.WriteValidationMedia(ctx, "pub-aging", []byte("payload")); err != nil {
		t.Fatalf("write validation media failed: %v", err)
	}

	repo.EvictOldValidationMedia(time.Now().UTC().Add(2*time.Hour), time.Hour)

	payload, err := repo.ReadValidationMedia(ctx, "pub-aging")
	if err != nil {
		t.Fatalf("read validation media failed: %v", err)
	}
	if len(payload) != 0 {
		t.Fatalf("expected aged payload to be evicted, got len=%d", len(payload))
	}
}

func TestTargetRuntimeRepoEvictsAgedValidationMediaOnWrite(t *testing.T) {
	repo := NewTargetRuntimeRepo()
	ctx := context.Background()
	for _, pubID := range []string{"pub-old", "pub-new"} {
		pub, err := domain.NewTargetPublication(pubID, "pool", "lib", "drive", "cart", "iqn.2026-04.ai.holo:"+pubID)
		if err != nil {
			t.Fatalf("new publication failed: %v", err)
		}
		if err := repo.SavePublication(ctx, pub); err != nil {
			t.Fatalf("save publication failed: %v", err)
		}
	}
	if err := repo.WriteValidationMedia(ctx, "pub-old", []byte("old")); err != nil {
		t.Fatalf("write old validation media failed: %v", err)
	}
	repo.mu.Lock()
	repo.validationMediaWritten["pub-old"] = time.Now().UTC().Add(-2 * maxValidationMediaAge)
	repo.mu.Unlock()

	if err := repo.WriteValidationMedia(ctx, "pub-new", []byte("new")); err != nil {
		t.Fatalf("write new validation media failed: %v", err)
	}
	oldPayload, err := repo.ReadValidationMedia(ctx, "pub-old")
	if err != nil {
		t.Fatalf("read old validation media failed: %v", err)
	}
	if len(oldPayload) != 0 {
		t.Fatalf("expected old validation media to be evicted, got len=%d", len(oldPayload))
	}
}
