package store_test

import (
	"context"
	"errors"
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
	if err == nil || err.Error() != "license not found" {
		t.Fatalf("err = %v, want 'license not found'", err)
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
	if err == nil || err.Error() != "license not found" {
		t.Fatalf("err = %v, want 'license not found'", err)
	}
}
