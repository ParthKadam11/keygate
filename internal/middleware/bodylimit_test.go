package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// A body past the limit is refused, declared or not, and is never read
// whole: a declared one before the handler runs, a streamed one at the
// limit.
func TestBodyLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const limit = 1024
	var read int
	r := gin.New()
	r.Use(BodyLimit(limit))
	r.POST("/x", func(c *gin.Context) {
		n, err := io.Copy(io.Discard, c.Request.Body)
		read = int(n)
		if err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.Status(http.StatusTeapot)
	})
	call := func(body string, declared bool) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
		if !declared {
			req.ContentLength = -1
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	if code := call(strings.Repeat("a", limit), true); code != http.StatusTeapot {
		t.Errorf("body at the limit = %d, want it through", code)
	}
	read = -1
	if code := call(strings.Repeat("a", 10*limit), true); code != http.StatusRequestEntityTooLarge || read != -1 {
		t.Errorf("declared body past the limit = %d (handler read %d), want 413 before the handler", code, read)
	}
	if code := call(strings.Repeat("a", 10*limit), false); code != http.StatusBadRequest || read > limit {
		t.Errorf("streamed body past the limit = %d after reading %d, want refused within %d", code, read, limit)
	}
}
