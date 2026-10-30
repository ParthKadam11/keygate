package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// Retention across all products adds the products up day by day, as
// the snapshot chart does. Taken straight over the per-product rows, the
// largest product's total was set against whichever product had the
// most active licences, a pair no day ever had.
func TestRetentionDataAddsUpProducts(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	// A month no other test writes snapshots for, so the unfiltered
	// answer is this test's alone; still inside the window, which has
	// no upper bound.
	month := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	product := func(name string, days ...[2]int) *model.Product {
		t.Helper()
		p := &model.Product{Name: name, Slug: "retention-" + strings.ToLower(name) + "-" + suffix, Type: "desktop"}
		if err := s.CreateProduct(ctx, p); err != nil {
			t.Fatal(err)
		}
		// Deleting the product deletes its snapshots.
		t.Cleanup(func() {
			if _, err := s.DB.NewDelete().Model((*model.Product)(nil)).Where("id = ?", p.ID).Exec(context.Background()); err != nil {
				t.Error(err)
			}
		})
		for i, d := range days {
			if err := s.CreateAnalyticsSnapshot(ctx, &model.AnalyticsSnapshot{Date: month.AddDate(0, 0, i),
				ProductID: p.ID, TotalLicenses: d[0], ActiveLicenses: d[1]}); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	// [total, active] on the 1st and the 2nd of the month.
	a := product("A", [2]int{10, 2}, [2]int{10, 1})
	b := product("B", [2]int{4, 4}, [2]int{4, 3})

	for _, tc := range []struct {
		name, productID       string
		start, end            int
		retention, churnShare float64
	}{
		{"all products", "", 14, 6, 42.9, 57.1}, // the 1st: 10+4 licences, 2+4 active
		{"product A", a.ID, 10, 2, 20, 80},
		{"product B", b.ID, 4, 4, 100, 0},
	} {
		rows, err := s.GetRetentionData(ctx, tc.productID, 6)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var got *store.RetentionData
		for i := range rows {
			if rows[i].Period == "2099-01-01" {
				got = &rows[i]
			}
		}
		if got == nil {
			t.Errorf("%s: no row for 2099-01 in %+v", tc.name, rows)
			continue
		}
		if got.StartCount != tc.start || got.EndCount != tc.end || got.RetentionPct != tc.retention || got.ChurnPct != tc.churnShare {
			t.Errorf("%s: %+v, want start %d, end %d, retention %.1f, churn %.1f",
				tc.name, *got, tc.start, tc.end, tc.retention, tc.churnShare)
		}
	}
}
