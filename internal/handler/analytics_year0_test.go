package handler

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// A date Postgres cannot hold (year 0) is ignored like any other date
// that does not parse, not passed on to fail the query.
func TestAnalyticsIgnoreYearZero(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	for name, fn := range map[string]func(c *gin.Context){
		"analytics": h.ListAnalytics, "summary": h.AnalyticsSummary, "usage-top": h.AnalyticsUsageTop,
		"activation-trend": h.AnalyticsActivationTrend,
	} {
		for _, q := range []string{"?from=0000-01-01", "?to=0000-01-01"} {
			if code, out := f.call(fn, http.MethodGet, "/x"+q, "", nil); code != http.StatusOK {
				t.Errorf("%s%s = %d %v, want 200", name, q, code, out)
			}
		}
	}
	if code, out := f.call(h.AnalyticsBreakdown, http.MethodGet, "/x?dimension=status&from=0000-01-01", "", nil); code != http.StatusOK {
		t.Errorf("breakdown with year 0 = %d %v, want 200", code, out)
	}
}
