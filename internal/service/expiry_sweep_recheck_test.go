package service

import (
	"context"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// The sweeps re-check in the write: a licence extended (grace) or paid
// (past due) after the sweep read it is left, and nothing is announced.
// The upgrade hooks run between the read and the write, so they stand
// in for the operator here.
func TestSweeps_ChangedSinceReadIsNotAnnounced(t *testing.T) {
	f := newFallbackFixture(t)
	lic := f.licenseIn(f.paidPlan(2), model.StatusActive, ago(3*day))
	f.c.EndGraceExpiredUpgrade = func(ctx context.Context, l *model.License, _ time.Time) (bool, error) {
		if l.ID == lic.ID {
			later := time.Now().Add(30 * day)
			if _, err := f.s.DB.NewRaw("UPDATE licenses SET valid_until = ? WHERE id = ?", later, l.ID).Exec(ctx); err != nil {
				t.Fatal(err)
			}
		}
		return false, nil
	}
	f.c.ExpireGracePeriodLicenses(f.ctx)
	if got := f.load(lic).Status; got != model.StatusActive {
		t.Fatalf("grace: licence extended since the read is %s, want active", got)
	}
	time.Sleep(300 * time.Millisecond)
	if n := f.count("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'expired'", lic.ID); n != 0 {
		t.Fatalf("grace: %d expired audit lines for a licence that was not expired", n)
	}
	if n := f.deliveries(lic, model.EventLicenseExpired); n != 0 {
		t.Fatalf("grace: %d license.expired deliveries for a licence that was not expired", n)
	}

	pd := f.pastDue(ago(31*day), ago(31*day))
	f.c.EndPastDueUpgrade = func(ctx context.Context, l *model.License, _ time.Time) (bool, error) {
		if l.ID == pd.ID {
			if _, err := f.s.DB.NewRaw("UPDATE licenses SET status = 'active', past_due_at = NULL WHERE id = ?", l.ID).Exec(ctx); err != nil {
				t.Fatal(err)
			}
		}
		return false, nil
	}
	f.c.MarkPastDueAsExpired(f.ctx)
	if got := f.load(pd).Status; got != model.StatusActive {
		t.Fatalf("past due: licence paid since the read is %s, want active", got)
	}
	time.Sleep(300 * time.Millisecond)
	if n := f.count("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'expired'", pd.ID); n != 0 {
		t.Fatalf("past due: %d expired audit lines for a licence that was paid", n)
	}
}

// The grace is the plan's when the licence is written, not when it was
// read: raised meanwhile, a licence now within it stays active.
func TestGraceSweep_GraceRaisedSinceReadIsKept(t *testing.T) {
	f := newFallbackFixture(t)
	plan := f.paidPlan(2)
	raised := f.licenseIn(plan, model.StatusActive, ago(3*day))
	f.c.EndGraceExpiredUpgrade = func(ctx context.Context, l *model.License, _ time.Time) (bool, error) {
		if l.ID == raised.ID {
			if _, err := f.s.DB.NewRaw("UPDATE plans SET grace_days = 10 WHERE id = ?", l.PlanID).Exec(ctx); err != nil {
				t.Fatal(err)
			}
		}
		return false, nil
	}
	f.c.ExpireGracePeriodLicenses(f.ctx)
	if got := f.load(raised).Status; got != model.StatusActive {
		t.Fatalf("licence within the grace raised since the read is %s, want active", got)
	}

	// Past the raised grace too, it goes.
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET valid_until = ? WHERE id = ?", ago(11*day), raised.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.c.ExpireGracePeriodLicenses(f.ctx)
	if got := f.load(raised).Status; got != model.StatusExpired {
		t.Fatalf("licence past the raised grace is %s, want expired", got)
	}
}
