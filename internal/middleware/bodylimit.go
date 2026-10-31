package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// MaxRequestBody is the largest request body any route takes. Every
// request the API serves is a small JSON document: release artifacts
// go straight to storage through a presigned URL, and a Stripe webhook
// event is far below it (Stripe's own examples cap theirs at 64 KiB).
// Without a cap a single unauthenticated call (/license/verify, say)
// with a body of a hundred megabytes was read whole into memory.
const MaxRequestBody = 1 << 20

// BodyLimit refuses a request whose body is larger than limit: one that
// declares a larger Content-Length is answered 413 before anything is
// read, and any other body stops being read at the limit, so the
// handler fails to parse it instead of buffering it.
func BodyLimit(limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > limit {
			abortWithError(c, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE",
				"request body is too large")
			return
		}
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		}
		c.Next()
	}
}
