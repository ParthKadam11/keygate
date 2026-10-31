package handler

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// A shared cache keeps the public feed no longer than the download links
// in it stay valid.
func TestPublicFeedCachedNoLongerThanItsLinks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for ttl, want := range map[time.Duration]string{
		24 * time.Hour:   "public, max-age=60",
		30 * time.Second: "public, max-age=30",
		0:                "public, max-age=60",
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		(&ReleasePublicHandler{feedTTL: ttl}).writeFeedCacheHeaders(c, feedRequest{})
		if got := w.Header().Get("Cache-Control"); got != want {
			t.Errorf("feed URL TTL %s: Cache-Control = %q, want %q", ttl, got, want)
		}
	}
}
