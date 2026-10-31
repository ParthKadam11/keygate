package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

func TestRevokeLicense_DeletesActivationsAndFloatingSessions(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)

	act := &model.Activation{
		LicenseID:      lic.ID,
		Identifier:     "machine-1",
		IdentifierType: "device",
	}
	if err := s.CreateActivation(ctx, act); err != nil {
		t.Fatalf("create activation: %v", err)
	}

	sess := &model.FloatingSession{
		LicenseID:  lic.ID,
		Identifier: "machine-1",
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	if err := s.CheckOutFloating(ctx, sess); err != nil {
		t.Fatalf("checkout floating: %v", err)
	}

	// Verify activation and session exist before revoking
	actCount, err := s.CountActivations(ctx, lic.ID)
	if err != nil || actCount != 1 {
		t.Fatalf("expected 1 activation, got %d (err: %v)", actCount, err)
	}
	sessCount, err := s.DB.NewSelect().Model((*model.FloatingSession)(nil)).Where("license_id = ?", lic.ID).Count(ctx)
	if err != nil || sessCount != 1 {
		t.Fatalf("expected 1 floating session, got %d (err: %v)", sessCount, err)
	}

	if err := s.RevokeLicense(ctx, lic.ID); err != nil {
		t.Fatalf("RevokeLicense: %v", err)
	}

	if st, _ := licenseState(t, s, ctx, lic.ID); st != model.StatusRevoked {
		t.Errorf("license status = %s, want %s", st, model.StatusRevoked)
	}
	if st, _ := subscriptionState(t, s, ctx, lic.ID); st != model.StatusRevoked {
		t.Errorf("subscription status = %s, want %s", st, model.StatusRevoked)
	}

	actCount, err = s.CountActivations(ctx, lic.ID)
	if err != nil || actCount != 0 {
		t.Errorf("expected 0 activations after revoke, got %d (err: %v)", actCount, err)
	}
	sessCount, err = s.DB.NewSelect().Model((*model.FloatingSession)(nil)).Where("license_id = ?", lic.ID).Count(ctx)
	if err != nil || sessCount != 0 {
		t.Errorf("expected 0 floating sessions after revoke, got %d (err: %v)", sessCount, err)
	}
}

func TestRevokeLicense_NotFound(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	err := s.RevokeLicense(ctx, "nonexistent-id")
	if !errors.Is(err, store.ErrLicenseNotFound) {
		t.Fatalf("err = %v, want ErrLicenseNotFound", err)
	}
}

func TestSuspendLicense_SyncsSubscription(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)

	if err := s.SuspendLicense(ctx, lic.ID); err != nil {
		t.Fatalf("SuspendLicense: %v", err)
	}

	if st, _ := licenseState(t, s, ctx, lic.ID); st != model.StatusSuspended {
		t.Errorf("license status = %s, want %s", st, model.StatusSuspended)
	}
	if st, _ := subscriptionState(t, s, ctx, lic.ID); st != model.StatusSuspended {
		t.Errorf("subscription status = %s, want %s", st, model.StatusSuspended)
	}

	// Re-suspending an already suspended license should return ErrLicenseNotSuspendable
	err := s.SuspendLicense(ctx, lic.ID)
	if !errors.Is(err, store.ErrLicenseNotSuspendable) {
		t.Errorf("err = %v, want ErrLicenseNotSuspendable", err)
	}
}

func TestSuspendLicense_NotFound(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	err := s.SuspendLicense(ctx, "nonexistent-id")
	if !errors.Is(err, store.ErrLicenseNotFound) {
		t.Fatalf("err = %v, want ErrLicenseNotFound", err)
	}
}

// A failed subscription update undoes the whole revoke: the licence is
// not left revoked with its devices gone and its subscription active.
func TestRevokeLicense_FailedSubscriptionSyncRollsBack(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)
	if err := s.CreateActivation(ctx, &model.Activation{LicenseID: lic.ID, Identifier: "machine-1", IdentifierType: "device"}); err != nil {
		t.Fatal(err)
	}

	fn := "fail_revoke_" + strings.ReplaceAll(lic.ID, "-", "")
	if _, err := s.DB.ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'database unavailable'; END $$`, fn)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.DB.ExecContext(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON subscriptions", fn))
		_, _ = s.DB.ExecContext(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", fn))
	})
	if _, err := s.DB.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON subscriptions FOR EACH ROW
		WHEN (NEW.license_id = '%s') EXECUTE FUNCTION %s()`, fn, lic.ID, fn)); err != nil {
		t.Fatal(err)
	}

	err := s.RevokeLicense(ctx, lic.ID)
	if !strings.Contains(fmt.Sprint(err), "database unavailable") {
		t.Fatalf("err = %v, want the trigger's error", err)
	}
	if st, _ := licenseState(t, s, ctx, lic.ID); st != model.StatusActive {
		t.Errorf("license status = %s, want it still active", st)
	}
	if st, _ := subscriptionState(t, s, ctx, lic.ID); st != model.StatusActive {
		t.Errorf("subscription status = %s, want it still active", st)
	}
	if n, err := s.CountActivations(ctx, lic.ID); err != nil || n != 1 {
		t.Errorf("activations = %d (err %v), want the device kept", n, err)
	}
}

// An activation or floating checkout that read the licence usable before
// it was revoked or suspended is refused under the licence lock: the
// store sees the licence as it is now, not as the caller found it.
func TestActivateAndCheckOut_RefusedOnceRevokedOrSuspended(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	roomy := func(lic *model.License) *model.License {
		t.Helper()
		if _, err := s.DB.NewRaw("UPDATE plans SET max_activations = 5 WHERE id = ?", lic.PlanID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		return lic
	}
	activate := func(lic *model.License, device string) error {
		return s.ActivateForKey(ctx, &model.Activation{LicenseID: lic.ID, Identifier: device, IdentifierType: "device"}, 5, lic.LicenseKey)
	}
	checkOut := func(lic *model.License, machine string) error {
		_, err := s.CheckOutFloatingForKey(ctx, &model.FloatingSession{LicenseID: lic.ID, Identifier: machine,
			ExpiresAt: time.Now().Add(time.Hour)}, 5, lic.LicenseKey)
		return err
	}

	revoked := roomy(newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil))
	if err := activate(revoked, "before"); err != nil {
		t.Fatalf("activate while active: %v", err)
	}
	if err := s.RevokeLicense(ctx, revoked.ID); err != nil {
		t.Fatal(err)
	}
	if err := activate(revoked, "after"); !errors.Is(err, store.ErrKeyRevoked) {
		t.Errorf("activate after revoke: err = %v, want ErrKeyRevoked", err)
	}
	if err := checkOut(revoked, "after"); !errors.Is(err, store.ErrKeyRevoked) {
		t.Errorf("checkout after revoke: err = %v, want ErrKeyRevoked", err)
	}
	if n, _ := s.CountActivations(ctx, revoked.ID); n != 0 {
		t.Errorf("revoked license has %d activations, want 0", n)
	}

	suspended := roomy(newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil))
	if err := activate(suspended, "before"); err != nil {
		t.Fatalf("activate while active: %v", err)
	}
	if err := s.SuspendLicense(ctx, suspended.ID); err != nil {
		t.Fatal(err)
	}
	if err := activate(suspended, "after"); !errors.Is(err, store.ErrKeyRevoked) {
		t.Errorf("activate after suspend: err = %v, want ErrKeyRevoked", err)
	}
	if err := checkOut(suspended, "after"); !errors.Is(err, store.ErrKeyRevoked) {
		t.Errorf("checkout after suspend: err = %v, want ErrKeyRevoked", err)
	}
	// Reinstated, it takes devices again; the one from before stayed.
	if _, err := s.ReinstateLicense(ctx, suspended.ID); err != nil {
		t.Fatal(err)
	}
	if err := activate(suspended, "after"); err != nil {
		t.Errorf("activate after reinstate: %v", err)
	}
	if err := checkOut(suspended, "after"); err != nil {
		t.Errorf("checkout after reinstate: %v", err)
	}
	if n, _ := s.CountActivations(ctx, suspended.ID); n != 2 {
		t.Errorf("reinstated license has %d activations, want 2", n)
	}
}
