package payment

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v82"

	"github.com/tabloy/keygate/internal/model"
)

// A licence issued for a subscription starts as the subscription is:
// trialing while it is, and valid to the end of the period it bills, not
// open-ended. The invoice that pays the first period usually arrives
// before the licence exists and finds nothing to update.
func TestSubscriptionLicenceStartsFromItsSubscription(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	prod := &model.Product{Name: "Start", Slug: "start-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	price := "price_start_" + suffix
	plan := &model.Plan{ProductID: prod.ID, Name: "Monthly", Slug: "start-" + suffix, LicenseType: "subscription",
		LicenseModel: "standard", BillingInterval: "month", StripePriceID: price}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	trialEnd := time.Now().Add(14 * 24 * time.Hour).Truncate(time.Second)
	firstEnd := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	laterEnd := time.Now().Add(31 * 24 * time.Hour).Truncate(time.Second)

	var mu sync.Mutex
	reads := map[string]int{}
	// What Stripe answers for each read of a subscription: the first is
	// fulfilment's, the second the read once the licence exists.
	answers := map[string][]string{}
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/")
		mu.Lock()
		n := reads[id]
		reads[id]++
		mu.Unlock()
		if r.Method != http.MethodGet || n >= len(answers[id]) || answers[id][n] == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"resource_missing"}}`))
			return
		}
		_, _ = w.Write([]byte(answers[id][n]))
	})
	sub := func(id, status string, end time.Time) string {
		return fmt.Sprintf(`{"id":%q,"object":"subscription","status":%q,"cancel_at_period_end":false,
			"items":{"object":"list","data":[{"id":"si_1","quantity":1,"current_period_end":%d,"price":{"id":%q,"object":"price"}}]}}`,
			id, status, end.Unix(), price)
	}
	h := &StripeHandler{Store: s}
	issue := func(name string) *model.License {
		t.Helper()
		subID := "sub_start_" + name + "_" + suffix
		if !fulfilOK(h, ctx, name+"-"+suffix+"@example.com", "", subID, "", map[string]string{"session_id": "cs_start_" + name + "_" + suffix}, "test") {
			t.Fatalf("%s: not fulfilled", name)
		}
		lic, err := s.FindLicenseByStripeSubscription(ctx, subID)
		if err != nil {
			t.Fatal(err)
		}
		return lic
	}

	// A trial, and the read once issued fails: the first read stands.
	answers["sub_start_trial_"+suffix] = []string{sub("sub_start_trial_"+suffix, "trialing", trialEnd)}
	if lic := issue("trial"); lic.Status != model.StatusTrialing || lic.ValidUntil == nil || !lic.ValidUntil.Equal(trialEnd) {
		t.Errorf("trial: %s until %v, want trialing until %v", lic.Status, lic.ValidUntil, trialEnd)
	}

	// Paid, and the read once issued finds it moved on: that read is
	// what the licence keeps.
	answers["sub_start_paid_"+suffix] = []string{sub("sub_start_paid_"+suffix, "active", firstEnd), sub("sub_start_paid_"+suffix, "active", laterEnd)}
	if lic := issue("paid"); lic.Status != model.StatusActive || lic.ValidUntil == nil || !lic.ValidUntil.Equal(laterEnd) {
		t.Errorf("paid: %s until %v, want active until %v", lic.Status, lic.ValidUntil, laterEnd)
	}

	// Cancelled in Stripe before the licence existed, its deletion
	// dropped for want of a licence: the read once issued ends it.
	answers["sub_start_gone_"+suffix] = []string{sub("sub_start_gone_"+suffix, "active", firstEnd), sub("sub_start_gone_"+suffix, "canceled", firstEnd)}
	if lic := issue("gone"); lic.Status != model.StatusCanceled || lic.ValidUntil == nil || lic.ValidUntil.After(time.Now()) {
		t.Errorf("cancelled before issue: %s until %v, want canceled and ended", lic.Status, lic.ValidUntil)
	}
}

// The success page's check sees the licence through once begun, even if
// the buyer closes the page: nothing else would retry what a cancelled
// request left out.
func TestVerifyCheckoutOutlivesTheRequest(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedPlan(t, s, ctx, "verifyclosed", "perpetual")
	sessionID := "cs_verifyclosed_" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	email := "closed-" + sessionID + "@example.com"
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/checkout/sessions/"+sessionID {
			fmt.Fprintf(w, `{"id":%q,"object":"checkout.session","mode":"payment","status":"complete","payment_status":"paid","customer_details":{"email":%q},"metadata":{"plan_id":%q}}`,
				sessionID, email, plan.ID)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"resource_missing"}}`))
	})
	h := &StripeHandler{Store: s}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	closed, closePage := context.WithCancel(context.Background())
	closePage()
	c.Request = httptest.NewRequest(http.MethodGet, "/checkout/verify?session_id="+sessionID, nil).WithContext(closed)
	h.VerifyCheckoutSession(c)
	if w.Code != http.StatusOK {
		t.Fatalf("verify = %d %s, want 200", w.Code, w.Body.String())
	}
	if _, err := s.FindLicenseByStripeCheckoutSession(ctx, sessionID); err != nil {
		t.Fatalf("no licence for the session once the page closed: %v", err)
	}
}

// A metered price bills the usage reported to its meter. Stripe refuses
// a quantity for it, so the checkout carries none, and such a plan is
// not sold per seat.
func TestCheckoutOfAMeteredPrice(t *testing.T) {
	metered := &stripe.Price{Recurring: &stripe.PriceRecurring{UsageType: stripe.PriceRecurringUsageTypeMetered}}
	licensed := &stripe.Price{Recurring: &stripe.PriceRecurring{UsageType: stripe.PriceRecurringUsageTypeLicensed}}
	if !priceMetered(metered) || priceMetered(licensed) || priceMetered(&stripe.Price{}) || priceMetered(nil) {
		t.Fatal("priceMetered tells a metered price wrongly")
	}

	f := newSeatFixture(t, 1)
	usage := map[string]string{f.solo.StripePriceID: "metered", f.team.StripePriceID: "metered"}
	var sessions []url.Values
	f.fake.extra = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/prices/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/prices/")
			fmt.Fprintf(w, `{"id":%q,"object":"price","type":"recurring","active":true,"recurring":{"interval":"month","usage_type":%q}}`, id, usage[id])
		case r.URL.Path == "/v1/checkout/sessions":
			_ = r.ParseForm()
			sessions = append(sessions, r.PostForm)
			_, _ = w.Write([]byte(`{"id":"cs_test","object":"checkout.session","url":"https://checkout.stripe.test/cs_test"}`))
		default:
			return false
		}
		return true
	}
	pay := func(plan *model.Plan, query string) int {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/pay/"+plan.CheckoutID+query, nil)
		c.Params = gin.Params{{Key: "checkout_id", Value: plan.CheckoutID}}
		f.h.CheckoutByPlan(c)
		return w.Code
	}
	if code := pay(f.solo, ""); code != http.StatusTemporaryRedirect {
		t.Fatalf("/pay of a metered plan = %d, want a redirect to Stripe", code)
	}
	form := sessions[len(sessions)-1]
	if form.Get("line_items[0][price]") != f.solo.StripePriceID || form.Has("line_items[0][quantity]") {
		t.Fatalf("metered line item = %v, want the price and no quantity", form)
	}
	if code := pay(f.team, ""); code != http.StatusBadRequest {
		t.Fatalf("/pay of a metered plan sold per seat = %d, want 400", code)
	}
	if code := pay(f.solo, "?quantity=2"); code != http.StatusBadRequest {
		t.Fatalf("/pay of 2 on a metered plan = %d, want 400", code)
	}
}
