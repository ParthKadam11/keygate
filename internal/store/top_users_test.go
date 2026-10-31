package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// A customer's usage in Analytics Insights is what their licences
// recorded, however many devices those licences run on. Joining the
// devices row by row counted a licence's usage once per device, so a
// customer on three machines showed three times their usage and was
// ranked above one who had used more.
func TestTopUsersCountsUsageOncePerLicense(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	prod := &model.Product{Name: "Top users", Slug: "top-users-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Pro", Slug: "top-users-pro-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	n := 0
	licence := func(email string, usage int64, devices int) {
		t.Helper()
		n++
		l := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: email,
			LicenseKey: "KG-TOP-" + suffix + "-" + string(rune('a'+n)), Status: model.StatusActive}
		if err := s.CreateLicense(ctx, l); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordUsageEvent(ctx, &model.UsageEvent{LicenseID: l.ID, Feature: "exports", Quantity: usage}); err != nil {
			t.Fatal(err)
		}
		for i := range devices {
			if err := s.CreateActivation(ctx, &model.Activation{LicenseID: l.ID,
				Identifier: l.LicenseKey + "-device-" + string(rune('a'+i)), IdentifierType: "device"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	many := "many-devices-" + suffix + "@example.com"
	heavy := "heavy-user-" + suffix + "@example.com"
	two := "two-licences-" + suffix + "@example.com"
	licence(many, 100, 3)
	licence(heavy, 150, 0)
	licence(two, 10, 2)
	licence(two, 5, 0)

	top, err := s.GetTopUsers(ctx, prod.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		licences, devices int
		usage             int64
	}
	got := map[string]row{}
	order := []string{}
	for _, u := range top {
		got[u.Email] = row{u.LicenseCount, u.ActivationCount, u.TotalUsage}
		order = append(order, u.Email)
	}
	for email, want := range map[string]row{
		many:  {1, 3, 100},
		heavy: {1, 0, 150},
		two:   {2, 2, 15},
	} {
		if got[email] != want {
			t.Errorf("%s: %+v, want %+v", email, got[email], want)
		}
	}
	// Two licences first, then by usage.
	if want := []string{two, heavy, many}; strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("ranking %v, want %v", order, want)
	}
}

// A customer is one row whether or not each of their licences is linked
// to their account: an admin-issued licence (no account yet) and one
// they took through self-signup (linked) are the same person, as the
// customer page takes them, by email.
func TestTopUsersOneRowPerCustomer(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	prod := &model.Product{Name: "Top users one row", Slug: "top-users-row-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Pro", Slug: "top-users-row-pro-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	email := "split-" + suffix + "@example.com"
	user := &model.User{ID: "u-split-" + suffix, Email: email, Role: "user"}
	if _, err := s.DB.NewInsert().Model(user).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	for i, l := range []*model.License{
		{Email: strings.ToUpper(email[:1]) + email[1:]}, // admin-issued, no account
		{Email: email, UserID: user.ID},                 // self-signup, linked
	} {
		l.ProductID, l.PlanID, l.Status = prod.ID, plan.ID, model.StatusActive
		l.LicenseKey = "KG-ROW-" + suffix + "-" + string(rune('a'+i))
		if err := s.CreateLicense(ctx, l); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordUsageEvent(ctx, &model.UsageEvent{LicenseID: l.ID, Feature: "exports", Quantity: int64(10 * (i + 1))}); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateActivation(ctx, &model.Activation{LicenseID: l.ID, Identifier: l.LicenseKey + "-pc", IdentifierType: "device"}); err != nil {
			t.Fatal(err)
		}
	}

	top, err := s.GetTopUsers(ctx, prod.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 {
		t.Fatalf("top users = %+v, want one row for the customer", top)
	}
	if u := top[0]; u.LicenseCount != 2 || u.TotalUsage != 30 || u.ActivationCount != 2 || u.UserID != user.ID {
		t.Errorf("customer row = %+v, want 2 licences, 30 usage, 2 devices, account %s", u, user.ID)
	}
}
