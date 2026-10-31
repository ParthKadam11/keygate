package handler

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
)

// A plan and its product's type are checked against each other where
// they are written, not only where the request read them: a type change
// that lands between the read and the write of a plan refuses the plan,
// and a plan that lands between the read and the write of a type change
// refuses the change. Without that a desktop product ends up with a plan
// selling seats.
func TestPlanAndProductTypeCheckedWhereWritten(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s, FeedURLTTL: time.Hour}
	hybrid := func(tag string) *model.Product {
		p := &model.Product{Name: tag, Slug: "shape-" + tag + "-" + f.suffix, Type: "hybrid"}
		if err := f.s.CreateProduct(f.ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	productType := func(p *model.Product) string {
		got, err := f.s.FindProductByID(f.ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Type
	}

	// The type changes after CreatePlan checked it.
	retyped := hybrid("a")
	h.beforeCutoffWrite = func() {
		if _, err := f.s.DB.NewRaw("UPDATE products SET type = 'desktop' WHERE id = ?", retyped.ID).Exec(f.ctx); err != nil {
			t.Error(err)
		}
	}
	body := fmt.Sprintf(`{"product_id":%q,"name":"Team","slug":"shape-team-%s","license_type":"perpetual","max_seats":3}`, retyped.ID, f.suffix)
	if code, out := f.call(h.CreatePlan, http.MethodPost, "/admin/plans", body, nil); code != http.StatusBadRequest || errCode(out) != "INCOMPATIBLE_PRODUCT_TYPE" {
		t.Errorf("plan with seats on a product made desktop meanwhile = %d %v, want 400 INCOMPATIBLE_PRODUCT_TYPE", code, out)
	}
	var plans int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM plans WHERE product_id = ?", retyped.ID).Scan(f.ctx, &plans); err != nil || plans != 0 {
		t.Errorf("plans on it = %d, %v; want none", plans, err)
	}

	// A plan with seats appears after UpdateProduct checked the plans.
	seated := hybrid("b")
	h.beforeCutoffWrite = func() {
		plan := &model.Plan{ProductID: seated.ID, Name: "Team", Slug: "shape-late-" + f.suffix, LicenseType: "perpetual",
			LicenseModel: "standard", MaxActivations: 3, MaxSeats: 3, Active: true}
		if err := f.s.CreatePlan(f.ctx, plan); err != nil {
			t.Error(err)
		}
	}
	if code, out := f.call(h.UpdateProduct, http.MethodPut, "/", `{"type":"desktop"}`, nil, gin.Param{Key: "id", Value: seated.ID}); code != http.StatusConflict || errCode(out) != "INCOMPATIBLE_PLANS" {
		t.Errorf("type change past a plan with seats made meanwhile = %d %v, want 409 INCOMPATIBLE_PLANS", code, out)
	}
	if got := productType(seated); got != "hybrid" {
		t.Errorf("product type = %s, want still hybrid", got)
	}

	// A release appears after UpdateProduct checked the releases.
	released := hybrid("c")
	h.beforeCutoffWrite = func() {
		rel := &model.Release{ProductID: released.ID, Version: "1.0.0", Channel: model.ReleaseChannelStable}
		if err := f.s.CreateRelease(f.ctx, rel); err != nil {
			t.Error(err)
		}
	}
	if code, out := f.call(h.UpdateProduct, http.MethodPut, "/", `{"type":"saas"}`, nil, gin.Param{Key: "id", Value: released.ID}); code != http.StatusBadRequest || errCode(out) != "INCOMPATIBLE_PRODUCT_TYPE" {
		t.Errorf("type change past a release made meanwhile = %d %v, want 400 INCOMPATIBLE_PRODUCT_TYPE", code, out)
	}
	if got := productType(released); got != "hybrid" {
		t.Errorf("product type = %s, want still hybrid", got)
	}
}

// A product deleted after UpdateProduct read it is a 404, not a 500.
func TestUpdateProductDeletedMeanwhile(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s, FeedURLTTL: time.Hour}
	gone := &model.Product{Name: "Gone", Slug: "shape-gone-" + f.suffix, Type: "desktop"}
	if err := f.s.CreateProduct(f.ctx, gone); err != nil {
		t.Fatal(err)
	}
	h.beforeCutoffWrite = func() {
		if err := f.s.DeleteProduct(f.ctx, gone.ID); err != nil {
			t.Error(err)
		}
	}
	if code, out := f.call(h.UpdateProduct, http.MethodPut, "/", `{"name":"Renamed"}`, nil, gin.Param{Key: "id", Value: gone.ID}); code != http.StatusNotFound {
		t.Errorf("updating a product deleted meanwhile = %d %v, want 404", code, out)
	}
}

// A plan made for a product deleted after the request checked it is a
// 404, not a 500.
func TestCreatePlanOnAProductDeletedMeanwhile(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s, FeedURLTTL: time.Hour}
	gone := &model.Product{Name: "Gone", Slug: "shape-plan-gone-" + f.suffix, Type: "desktop"}
	if err := f.s.CreateProduct(f.ctx, gone); err != nil {
		t.Fatal(err)
	}
	h.beforeCutoffWrite = func() {
		if err := f.s.DeleteProduct(f.ctx, gone.ID); err != nil {
			t.Error(err)
		}
	}
	body := fmt.Sprintf(`{"product_id":%q,"name":"Std","slug":"shape-gone-%s","license_type":"perpetual"}`, gone.ID, f.suffix)
	if code, out := f.call(h.CreatePlan, http.MethodPost, "/admin/plans", body, nil); code != http.StatusNotFound {
		t.Errorf("plan on a product deleted meanwhile = %d %v, want 404", code, out)
	}
}
