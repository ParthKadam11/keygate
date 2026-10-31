package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// A release made while the product's type changes waits for the change,
// then reads the new type: one made on a product that just lost its
// releases is refused, not left unreachable.
func TestReleaseWaitsForATypeChange(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	prod := &model.Product{Name: "Shape " + suffix, Slug: "shape-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := store.ProductShapeLockIn(ctx, tx, prod.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE products SET type = 'saas' WHERE id = ?", prod.ID); err != nil {
		t.Fatal(err)
	}
	made := make(chan error, 1)
	go func() {
		made <- s.CreateRelease(ctx, &model.Release{ProductID: prod.ID, Version: "1.0.0", Channel: model.ReleaseChannelStable})
	}()
	select {
	case err := <-made:
		t.Fatalf("release made while the type change held the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-made; !errors.Is(err, store.ErrProductNoReleases) {
		t.Fatalf("release on a product made saas meanwhile = %v, want ErrProductNoReleases", err)
	}
}

// The newest published version is by semver, per product and channel,
// whatever order the releases were made in.
func TestLatestPublishedVersions(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	prod := &model.Product{Name: "Latest " + suffix, Slug: "latest-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ version, channel, status string }{
		{"1.10.0", "stable", "published"}, {"1.9.0", "stable", "published"},
		{"2.0.0", "stable", "draft"}, {"3.0.0", "stable", "yanked"},
		{"2.0.0-beta.2", "beta", "published"}, {"2.0.0-beta.10", "beta", "published"},
	} {
		rel := &model.Release{ProductID: prod.ID, Version: r.version, Channel: r.channel}
		if err := s.CreateRelease(ctx, rel); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.NewRaw("UPDATE releases SET status = ?, published_at = now() WHERE id = ?", r.status, rel.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LatestPublishedVersions(ctx, prod.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"stable": "1.10.0", "beta": "2.0.0-beta.10"}
	if len(got) != len(want) {
		t.Fatalf("latest = %+v, want %v", got, want)
	}
	for _, l := range got {
		if want[l.Channel] != l.Version {
			t.Errorf("latest on %s = %s, want %s", l.Channel, l.Version, want[l.Channel])
		}
	}
}

// A plan write that takes the shape lock before its plan's row lock, as
// updatePlan does, waits for a type change instead of crossing it: the
// type change rewrites the product's per-seat plans under the product's
// row lock, and the plan write reaches for the product after its plan.
// Without the shape lock first this deadlocked.
func TestPlanWriteAndTypeChangeDoNotDeadlock(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	prod := &model.Product{Name: "DL " + suffix, Slug: "dl-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Seat", Slug: "seat-" + suffix, LicenseType: "subscription",
		LicenseModel: "standard", PerSeat: true, MinQuantity: 1, MaxQuantity: 10, MaxSeats: 1, Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	planTx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer planTx.Rollback() //nolint:errcheck
	if err := store.ProductShapeLockIn(ctx, planTx, prod.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := store.LockPlanIn(ctx, planTx, plan.ID); err != nil {
		t.Fatal(err)
	}
	changed := make(chan error, 1)
	go func() {
		changed <- store.RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
			if err := store.ProductShapeLockIn(ctx, tx, prod.ID, true); err != nil {
				return err
			}
			p, err := store.LockProductIn(ctx, tx, prod.ID)
			if err != nil {
				return err
			}
			p.Type = "desktop"
			return store.UpdateProductIn(ctx, tx, p, "type")
		})
	}()
	time.Sleep(300 * time.Millisecond)
	if err := store.LockReferencedRowsIn(ctx, planTx, "", prod.ID); err != nil {
		t.Fatalf("plan write reaching for the product: %v", err)
	}
	if err := planTx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-changed; err != nil {
		t.Fatalf("type change after the plan write: %v", err)
	}
	var seats int
	if err := s.DB.NewRaw("SELECT max_seats FROM plans WHERE id = ?", plan.ID).Scan(ctx, &seats); err != nil || seats != 0 {
		t.Fatalf("per-seat plan on a desktop product has max_seats %d, %v; want 0", seats, err)
	}
}
