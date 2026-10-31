package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// After ActivateForKey reports already activated, a concurrent release
// must make TouchActivation report the row gone so Activate does not
// sign a token for a freed slot.
func TestTouchActivation_FalseAfterRelease(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Cap", Slug: "act-touch-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "One", Slug: "act-touch-" + suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 1, Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	key := "KG-ACTTOUCH-" + suffix
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "touch-" + suffix + "@example.com",
		LicenseKey: key, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ls := NewLicenseService(s, priv, slog.Default(), nil, nil)
	if _, err := ls.Activate(ctx, ActivateInput{LicenseKey: key, Identifier: "device-a"}); err != nil {
		t.Fatal(err)
	}
	// Repeat activate goes through ActivateForKey (no unlocked shortcut).
	again, err := ls.Activate(ctx, ActivateInput{LicenseKey: key, Identifier: "device-a"})
	if err != nil || again.Status != "already_activated" || again.Token == "" {
		t.Fatalf("repeat activate: status=%q err=%v", again.Status, err)
	}

	a, err := s.FindActivation(ctx, lic.ID, "device-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteActivation(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	ok, err := s.TouchActivation(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("TouchActivation after release: ok=true, want false so Activate will not sign")
	}

	// Freed slot goes to another device; the released one must not also hold a row.
	if _, err := ls.Activate(ctx, ActivateInput{LicenseKey: key, Identifier: "device-b"}); err != nil {
		t.Fatalf("activate device-b on freed slot: %v", err)
	}
	n, err := s.CountActivations(ctx, lic.ID)
	if err != nil || n != 1 {
		t.Fatalf("activations = %d (err %v), want 1", n, err)
	}
	if _, err := s.FindActivation(ctx, lic.ID, "device-a"); err == nil {
		t.Fatal("device-a still activated after release + device-b took the slot")
	}
}
