package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v82"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// meteredTestSetup wires a real store + a recordable fake Stripe
// dispatcher. Skips the test cleanly when TEST_DATABASE_URL isn't
// set, matching the pattern used by store integration tests.
type meteredTestSetup struct {
	store    *store.Store
	syncer   *MeteredBillingSyncer
	captured []stripeMeterEvent
	mu       sync.Mutex
	failNext error
}

func (m *meteredTestSetup) dispatch(_ context.Context, in stripeMeterEvent) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext != nil {
		err := m.failNext
		m.failNext = nil
		return "", err
	}
	m.captured = append(m.captured, in)
	return "evt_" + in.Identifier, nil
}

func setupMeteredTest(t *testing.T) (*meteredTestSetup, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// These tests assert exact push counts against a fake Stripe, so
	// the queue must start empty. Rows left behind by e2e runs (the
	// dev server pushes to real Stripe test mode and gets killed
	// mid-queue) would otherwise be pushed too. TEST_DATABASE_URL is a
	// disposable database by contract.
	if _, err := s.DB.NewRaw("DELETE FROM metered_billing WHERE synced = false").Exec(context.Background()); err != nil {
		t.Fatalf("clear metered queue: %v", err)
	}

	setup := &meteredTestSetup{store: s}
	setup.syncer = &MeteredBillingSyncer{
		store:       s,
		logger:      slog.Default(),
		dispatch:    setup.dispatch,
		maxAttempts: 3,
	}
	return setup, context.Background()
}

// seedMeteredLicense creates a product → plan → entitlement (with a
// stripe_meter_event_name) → license tied to a stripe_customer_id.
// Returns the license + entitlement event_name for use in
// dispatcher-shape assertions.
func seedMeteredLicense(t *testing.T, s *store.Store, ctx context.Context, eventName, stripeCustomerID string) (*model.License, *model.Entitlement) {
	t.Helper()
	suffix := time.Now().Format("150405.000") + "-" + strconv.Itoa(int(time.Now().UnixNano()%1000000))
	prod := &model.Product{Name: "Metered " + suffix, Slug: "metered-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("product: %v", err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Pro", Slug: "pro-" + suffix,
		LicenseType: "subscription", LicenseModel: "standard",
		GraceDays: 7, MaxActivations: 5,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("plan: %v", err)
	}
	ent := &model.Entitlement{
		PlanID: plan.ID, Feature: "api_calls",
		ValueType: "quota", Value: "1000", QuotaPeriod: "monthly",
		StripeMeterEventName: eventName,
	}
	if err := s.CreateEntitlement(ctx, ent); err != nil {
		t.Fatalf("entitlement: %v", err)
	}
	lic := &model.License{
		ProductID: prod.ID, PlanID: plan.ID,
		Email:            "metered-" + suffix + "@example.com",
		LicenseKey:       "KEY-metered-" + suffix,
		Status:           model.StatusActive,
		StripeCustomerID: stripeCustomerID,
	}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatalf("license: %v", err)
	}
	return lic, ent
}

// Happy path: a single enqueued event pushes once, marks synced,
// captures the external id.
func TestMeteredSync_HappyPath(t *testing.T) {
	tt, ctx := setupMeteredTest(t)
	lic, _ := seedMeteredLicense(t, tt.store, ctx, "api_calls_meter", "cus_TEST_123")

	if err := tt.store.InsertMeteredEvent(ctx, lic.ID, "api_calls", "2026-05", 7); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	pushed := tt.syncer.RunOnce(ctx, 10)
	if pushed != 1 {
		t.Fatalf("expected 1 push, got %d", pushed)
	}
	if len(tt.captured) != 1 {
		t.Fatalf("dispatcher should have seen one event, got %d", len(tt.captured))
	}
	ev := tt.captured[0]
	if ev.EventName != "api_calls_meter" || ev.CustomerID != "cus_TEST_123" || ev.Quantity != 7 {
		t.Fatalf("dispatcher payload off: %+v", ev)
	}
	if ev.Identifier == "" {
		t.Fatal("identifier must be non-empty for Stripe dedup")
	}

	// Row marked synced + external_id set.
	rows, _ := tt.store.ListMeteredBilling(ctx, lic.ID)
	if len(rows) != 1 || !rows[0].Synced {
		t.Fatalf("row should be synced: %+v", rows)
	}
	if rows[0].ExternalID == "" {
		t.Fatal("external_id should be recorded")
	}
}

// Re-running drains nothing (no unsynced rows).
func TestMeteredSync_Idempotent(t *testing.T) {
	tt, ctx := setupMeteredTest(t)
	lic, _ := seedMeteredLicense(t, tt.store, ctx, "ev", "cus_idem")
	_ = tt.store.InsertMeteredEvent(ctx, lic.ID, "api_calls", "2026-05", 1)
	tt.syncer.RunOnce(ctx, 10)
	tt.syncer.RunOnce(ctx, 10) // second call
	if len(tt.captured) != 1 {
		t.Fatalf("expected exactly 1 dispatch across two runs, got %d", len(tt.captured))
	}
}

// Failure: dispatcher returns an error → row stays unsynced,
// attempts increments, last_error populated. Next run retries.
func TestMeteredSync_FailureRetries(t *testing.T) {
	tt, ctx := setupMeteredTest(t)
	lic, _ := seedMeteredLicense(t, tt.store, ctx, "ev", "cus_fail")
	_ = tt.store.InsertMeteredEvent(ctx, lic.ID, "api_calls", "2026-05", 3)

	tt.failNext = errors.New("rate_limited")
	pushed := tt.syncer.RunOnce(ctx, 10)
	if pushed != 0 {
		t.Fatalf("failed push should report 0, got %d", pushed)
	}
	rows, _ := tt.store.ListMeteredBilling(ctx, lic.ID)
	if rows[0].Synced {
		t.Fatal("row should remain unsynced after failure")
	}
	if rows[0].Attempts != 1 {
		t.Fatalf("attempts=1 expected, got %d", rows[0].Attempts)
	}
	if rows[0].LastError == "" {
		t.Fatal("last_error should be populated")
	}

	// Next run with the dispatcher healthy → succeeds.
	pushed = tt.syncer.RunOnce(ctx, 10)
	if pushed != 1 {
		t.Fatalf("retry should push, got %d", pushed)
	}
}

// Attempts ceiling: once a row hits maxAttempts the syncer skips
// it. Without this, a poison-pill row would burn Stripe rate budget
// indefinitely.
func TestMeteredSync_MaxAttemptsCeiling(t *testing.T) {
	tt, ctx := setupMeteredTest(t)
	lic, _ := seedMeteredLicense(t, tt.store, ctx, "ev", "cus_max")
	_ = tt.store.InsertMeteredEvent(ctx, lic.ID, "api_calls", "2026-05", 5)

	// Manually wind attempts up to the ceiling. Use NewRaw to keep
	// the test SQL transparent — Model((*T)(nil)).Set("attempts = ?")
	// in bun produces an alias-prefixed column the optimiser
	// sometimes refuses.
	rows, _ := tt.store.ListMeteredBilling(ctx, lic.ID)
	if _, err := tt.store.DB.NewRaw(
		"UPDATE metered_billing SET attempts = 3 WHERE id = ?", rows[0].ID,
	).Exec(ctx); err != nil {
		t.Fatalf("seed ceiling attempts: %v", err)
	}

	tt.failNext = errors.New("should_not_be_called")
	pushed := tt.syncer.RunOnce(ctx, 10)
	if pushed != 0 {
		t.Fatalf("ceiling row should not push, got %d", pushed)
	}
	if len(tt.captured) != 0 {
		t.Fatal("dispatcher must not be called for capped row")
	}
	// failNext was untouched (no call) — confirms.
	if tt.failNext == nil {
		t.Fatal("dispatcher must not consume the failNext slot")
	}
}

// Edge case: license missing stripe_customer_id (self-hosted /
// non-Stripe). The row would otherwise loop forever; the syncer
// marks it synced with a sentinel external_id and moves on.
func TestMeteredSync_NoStripeCustomer(t *testing.T) {
	tt, ctx := setupMeteredTest(t)
	lic, _ := seedMeteredLicense(t, tt.store, ctx, "ev", "") // empty customer id
	_ = tt.store.InsertMeteredEvent(ctx, lic.ID, "api_calls", "2026-05", 2)
	pushed := tt.syncer.RunOnce(ctx, 10)
	if pushed != 0 {
		t.Fatalf("no-customer row should not count as 'pushed', got %d", pushed)
	}
	rows, _ := tt.store.ListMeteredBilling(ctx, lic.ID)
	if !rows[0].Synced {
		t.Fatal("no-customer row should be marked synced to drain the queue")
	}
	if rows[0].ExternalID != "no_stripe_customer" {
		t.Fatalf("expected sentinel external_id, got %q", rows[0].ExternalID)
	}
}

// Edge case: entitlement's event_name was cleared after the row
// was enqueued. Same drain-the-queue treatment.
func TestMeteredSync_EventNameCleared(t *testing.T) {
	tt, ctx := setupMeteredTest(t)
	lic, ent := seedMeteredLicense(t, tt.store, ctx, "ev_was_set", "cus_X")
	_ = tt.store.InsertMeteredEvent(ctx, lic.ID, "api_calls", "2026-05", 4)

	// Wipe the event name to simulate admin reconfiguring after
	// enqueue. UpdateEntitlement is the natural path.
	ent.StripeMeterEventName = ""
	if err := tt.store.UpdateEntitlement(ctx, ent); err != nil {
		t.Fatalf("update entitlement: %v", err)
	}

	tt.syncer.RunOnce(ctx, 10)
	rows, _ := tt.store.ListMeteredBilling(ctx, lic.ID)
	if !rows[0].Synced || rows[0].ExternalID != "no_event_name" {
		t.Fatalf("row should drain with sentinel, got %+v", rows[0])
	}
}

// Legacy row: identifier empty (pre-migration aggregate-style row).
// Drained with a sentinel so the queue doesn't stall on historical
// data the new dispatcher can't safely push.
func TestMeteredSync_LegacyEmptyIdentifier(t *testing.T) {
	tt, ctx := setupMeteredTest(t)
	lic, _ := seedMeteredLicense(t, tt.store, ctx, "ev", "cus_L")

	// Insert manually so we can leave identifier empty.
	row := &model.MeteredBilling{
		ID:        "legacy-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		LicenseID: lic.ID, Feature: "api_calls", Quantity: 99, PeriodKey: "2026-04",
	}
	if _, err := tt.store.DB.NewInsert().Model(row).Exec(ctx); err != nil {
		t.Fatalf("manual insert: %v", err)
	}

	tt.syncer.RunOnce(ctx, 10)
	rows, _ := tt.store.ListMeteredBilling(ctx, lic.ID)
	// The legacy row should be synced with the legacy sentinel.
	var found bool
	for _, r := range rows {
		if r.ID == row.ID {
			found = true
			if !r.Synced || r.ExternalID != "legacy_no_identifier" {
				t.Fatalf("legacy row drain sentinel wrong: %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("legacy row missing from listing")
	}
	if len(tt.captured) != 0 {
		t.Fatal("dispatcher must not be called for legacy rows")
	}
}

// A row that has exhausted its attempts must not occupy a slot in the
// batch: with created_at ordering, enough dead rows would starve every
// live one behind them.
func TestMeteredSync_DeadRowsDoNotStarveTheBatch(t *testing.T) {
	tt, ctx := setupMeteredTest(t)
	lic, _ := seedMeteredLicense(t, tt.store, ctx, "api_calls_meter", "cus_TEST_dead")

	// Two dead rows first (attempts at the ceiling), then one live row.
	for range 2 {
		if err := tt.store.InsertMeteredEvent(ctx, lic.ID, "api_calls", "2026-01", 1); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if _, err := tt.store.DB.NewRaw("UPDATE metered_billing SET attempts = ? WHERE license_id = ? AND synced = false", tt.syncer.maxAttempts, lic.ID).Exec(ctx); err != nil {
		t.Fatalf("age rows: %v", err)
	}
	if err := tt.store.InsertMeteredEvent(ctx, lic.ID, "api_calls", "2026-02", 5); err != nil {
		t.Fatalf("insert live: %v", err)
	}

	if pushed := tt.syncer.RunOnce(ctx, 2); pushed != 1 {
		t.Fatalf("limit 2 with 2 dead rows ahead: pushed %d, want 1 (the live row)", pushed)
	}
}

// Passes running at once on several replicas push each row once and
// count one attempt for it, not one per replica: one runs, the others
// find it running and leave the rows to it.
func TestMeteredSync_OnePassAtATime(t *testing.T) {
	tt, ctx := setupMeteredTest(t)
	lic, _ := seedMeteredLicense(t, tt.store, ctx, "ev_once", "cus_once")
	for range 5 {
		if err := tt.store.InsertMeteredEvent(ctx, lic.ID, "api_calls", "2026-05", 1); err != nil {
			t.Fatal(err)
		}
	}
	held := make(chan struct{})
	unblock := make(chan struct{})
	var once sync.Once
	tt.syncer.dispatch = func(ctx context.Context, in stripeMeterEvent) (string, error) {
		once.Do(func() { close(held); <-unblock })
		return tt.dispatch(ctx, in)
	}
	done := make(chan int, 1)
	go func() { done <- tt.syncer.RunOnce(ctx, 10) }()
	<-held // the first pass is pushing, and holds the lock
	other := &MeteredBillingSyncer{store: tt.store, logger: tt.syncer.logger, dispatch: tt.dispatch, maxAttempts: 3}
	if pushed := other.RunOnce(ctx, 10); pushed != 0 {
		t.Errorf("a second pass while the first runs pushed %d, want 0", pushed)
	}
	close(unblock)
	if pushed := <-done; pushed != 5 {
		t.Errorf("the first pass pushed %d, want 5", pushed)
	}
	if len(tt.captured) != 5 {
		t.Errorf("Stripe saw %d events, want each of the 5 once", len(tt.captured))
	}
	var attempts int
	if err := tt.store.DB.NewRaw("SELECT coalesce(sum(attempts), 0) FROM metered_billing WHERE license_id = ?", lic.ID).Scan(ctx, &attempts); err != nil || attempts > 5 {
		t.Errorf("attempts = %d, %v; want at most one per row", attempts, err)
	}
	// Released: the next pass runs.
	if err := tt.store.InsertMeteredEvent(ctx, lic.ID, "api_calls", "2026-05", 1); err != nil {
		t.Fatal(err)
	}
	if pushed := other.RunOnce(ctx, 10); pushed != 1 {
		t.Errorf("a pass after the first finished pushed %d, want 1", pushed)
	}
}

// A pass sends the whole queue, a batch at a time, not one batch; and a
// row that fails is tried once in the pass, not again for every batch.
func TestMeteredSync_SendsTheWholeQueue(t *testing.T) {
	tt, ctx := setupMeteredTest(t)
	lic, _ := seedMeteredLicense(t, tt.store, ctx, "ev_all", "cus_all")
	for range 25 {
		if err := tt.store.InsertMeteredEvent(ctx, lic.ID, "api_calls", "2026-05", 1); err != nil {
			t.Fatal(err)
		}
	}
	tt.failNext = errors.New("stripe: temporary")
	if pushed := tt.syncer.RunOnce(ctx, 10); pushed != 24 {
		t.Fatalf("pushed %d in one pass of batches of 10, want the 24 that did not fail", pushed)
	}
	var waiting, attempts int
	if err := tt.store.DB.NewRaw("SELECT count(*), coalesce(sum(attempts), 0) FROM metered_billing WHERE license_id = ? AND synced = false", lic.ID).Scan(ctx, &waiting, &attempts); err != nil {
		t.Fatal(err)
	}
	if waiting != 1 || attempts != 1 {
		t.Fatalf("waiting rows = %d with %d attempts, want the one that failed, tried once", waiting, attempts)
	}
	if pushed := tt.syncer.RunOnce(ctx, 10); pushed != 1 {
		t.Fatalf("the next pass pushed %d, want the one left", pushed)
	}
}

// Stripe failing every push ends the pass after a run of failures, not
// at the end of the queue: the rest waits for the next pass.
func TestMeteredSync_RunOfFailuresEndsThePass(t *testing.T) {
	tt, ctx := setupMeteredTest(t)
	lic, _ := seedMeteredLicense(t, tt.store, ctx, "ev_down", "cus_down")
	for range 30 {
		if err := tt.store.InsertMeteredEvent(ctx, lic.ID, "api_calls", "2026-05", 1); err != nil {
			t.Fatal(err)
		}
	}
	tries := 0
	tt.syncer.dispatch = func(context.Context, stripeMeterEvent) (string, error) {
		tries++
		return "", errors.New("stripe: unreachable")
	}
	tt.syncer.RunOnce(ctx, 5)
	if tries != meteredFailuresEndPass {
		t.Fatalf("pushes tried with Stripe down = %d, want the pass to end after %d", tries, meteredFailuresEndPass)
	}
}

// A run of rows Stripe refuses on their own merits (a customer it does
// not know) does not end the pass: the rows behind them, other
// customers' usage, are still sent in the same pass.
func TestMeteredSync_RefusedRowsDoNotHoldTheQueue(t *testing.T) {
	tt, ctx := setupMeteredTest(t)
	gone, _ := seedMeteredLicense(t, tt.store, ctx, "ev_gone", "cus_gone")
	good, _ := seedMeteredLicense(t, tt.store, ctx, "ev_good", "cus_good")
	for range 30 {
		if err := tt.store.InsertMeteredEvent(ctx, gone.ID, "api_calls", "2026-05", 1); err != nil {
			t.Fatal(err)
		}
	}
	for range 20 {
		if err := tt.store.InsertMeteredEvent(ctx, good.ID, "api_calls", "2026-05", 1); err != nil {
			t.Fatal(err)
		}
	}
	tt.syncer.dispatch = func(ctx context.Context, in stripeMeterEvent) (string, error) {
		if in.CustomerID == "cus_gone" {
			return "", &stripe.Error{HTTPStatusCode: http.StatusBadRequest, Msg: "No such customer"}
		}
		return tt.dispatch(ctx, in)
	}
	if pushed := tt.syncer.RunOnce(ctx, 10); pushed != 20 {
		t.Fatalf("pushed %d in one pass, want the good customer's 20 behind the refused 30", pushed)
	}
}

// The account's key refused (401, or 403 for a restricted key short of a
// permission) is Stripe refusing every row, not this one: it ends the
// pass after a run, rather than spending every queued row's retries.
func TestMeteredSync_KeyRefusedEndsThePass(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		tt, ctx := setupMeteredTest(t)
		lic, _ := seedMeteredLicense(t, tt.store, ctx, "ev_key", "cus_key")
		for range 30 {
			if err := tt.store.InsertMeteredEvent(ctx, lic.ID, "api_calls", "2026-05", 1); err != nil {
				t.Fatal(err)
			}
		}
		tries := 0
		tt.syncer.dispatch = func(context.Context, stripeMeterEvent) (string, error) {
			tries++
			return "", &stripe.Error{HTTPStatusCode: status, Msg: "Invalid API Key provided"}
		}
		tt.syncer.RunOnce(ctx, 5)
		if tries != meteredFailuresEndPass {
			t.Errorf("status %d: pushes tried = %d, want the pass to end after %d", status, tries, meteredFailuresEndPass)
		}
	}
}

// Rows drained without asking Stripe (no customer) do not end a run of
// Stripe failures: with Stripe down the pass still ends after a run.
func TestMeteredSync_DrainedRowsDoNotResetTheRun(t *testing.T) {
	tt, ctx := setupMeteredTest(t)
	billed, _ := seedMeteredLicense(t, tt.store, ctx, "ev_mixed", "cus_mixed")
	unbilled, _ := seedMeteredLicense(t, tt.store, ctx, "ev_none", "")
	for i := range 60 {
		lic := billed
		if i%5 == 4 {
			lic = unbilled
		}
		if err := tt.store.InsertMeteredEvent(ctx, lic.ID, "api_calls", "2026-05", 1); err != nil {
			t.Fatal(err)
		}
	}
	tries := 0
	tt.syncer.dispatch = func(context.Context, stripeMeterEvent) (string, error) {
		tries++
		return "", errors.New("stripe: unreachable")
	}
	tt.syncer.RunOnce(ctx, 7)
	if tries != meteredFailuresEndPass {
		t.Fatalf("pushes tried with Stripe down = %d, want %d despite the drained rows between them", tries, meteredFailuresEndPass)
	}
}
