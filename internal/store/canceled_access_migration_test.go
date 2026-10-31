package store_test

import (
	"context"
	"os"
	"testing"
)

// The migration that ends canceled licences at their cancellation moves
// an expiry still ahead of that moment back to it, and leaves alone one
// already behind, a licence not canceled, and one with no expiry. The
// file runs as written against a temporary table named licenses, which
// the unqualified name resolves to first, so the real licences of other
// tests are untouched.
func TestCanceledAccessEndsMigration(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	sql, err := os.ReadFile("../../db/migrations/20261102_canceled_access_ends.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE licenses (id text PRIMARY KEY, status text NOT NULL,
		canceled_at timestamptz, valid_until timestamptz, updated_at timestamptz) ON COMMIT DROP`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pg_temp.licenses (id, status, canceled_at, valid_until) VALUES
		('unpaid-year', 'canceled', now() - interval '2 days', now() + interval '338 days'),
		('period-end', 'canceled', now() - interval '2 days', now() - interval '3 days'),
		('no-stamp', 'canceled', NULL, now() + interval '30 days'),
		('no-expiry', 'canceled', now() - interval '2 days', NULL),
		('active', 'active', NULL, now() + interval '30 days')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"unpaid-year": "valid_until = canceled_at",
		"period-end":  "valid_until = canceled_at - interval '1 day'",
		"no-stamp":    "valid_until <= now()",
		"no-expiry":   "valid_until IS NULL",
		"active":      "valid_until > now() + interval '29 days'",
	} {
		var ok bool
		if err := tx.NewRaw("SELECT "+want+" FROM pg_temp.licenses WHERE id = ?", id).Scan(ctx, &ok); err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("%s: want %s", id, want)
		}
	}
}
