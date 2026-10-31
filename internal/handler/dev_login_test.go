package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/config"
)

// Dev-login is for the machine running a development server. A server
// left on the defaults (development, BASE_URL on localhost) and
// reachable from elsewhere must not hand an owner session to a caller
// there, whatever Host or forwarding header it sends.
func TestDevLoginOnlyFromThisMachine(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &AuthHandler{Config: &config.Config{Environment: "development", BaseURL: "http://localhost:9000"}}
	r := gin.New()
	r.GET("/providers", h.Providers)
	r.POST("/dev-login", h.DevLogin)
	callAs := func(method, path, peer, host string, header http.Header) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(`{"email":"x@example.com"}`))
		req.RemoteAddr = peer
		req.Host = host
		for k, v := range header {
			req.Header[k] = v
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	call := func(method, path, peer string) *httptest.ResponseRecorder {
		return callAs(method, path, peer, "localhost:9000", nil)
	}
	spoofed := http.Header{"X-Forwarded-For": {"127.0.0.1"}}
	if w := callAs(http.MethodPost, "/dev-login", "203.0.113.7:51000", "localhost:9000", spoofed); w.Code != http.StatusNotFound {
		t.Errorf("dev-login from afar claiming loopback = %d, want 404", w.Code)
	}
	// Behind a reverse proxy on this machine every peer is loopback; the
	// proxy gives itself away by the host it passes on or the header
	// naming whom it forwards for.
	for name, req := range map[string]struct {
		host   string
		header http.Header
	}{
		"public host":      {"licensing.example.com", nil},
		"X-Forwarded-For":  {"localhost:9000", http.Header{"X-Forwarded-For": {"198.51.100.4"}}},
		"Forwarded":        {"localhost:9000", http.Header{"Forwarded": {"for=198.51.100.4"}}},
		"X-Real-IP":        {"localhost:9000", http.Header{"X-Real-Ip": {"198.51.100.4"}}},
		"X-Forwarded-Host": {"localhost:9000", http.Header{"X-Forwarded-Host": {"licensing.example.com"}}},
		"LAN address":      {"192.168.1.20:9000", nil},
	} {
		if w := callAs(http.MethodPost, "/dev-login", "127.0.0.1:51000", req.host, req.header); w.Code != http.StatusNotFound {
			t.Errorf("dev-login through a local proxy (%s) = %d, want 404", name, w.Code)
		}
		if w := callAs(http.MethodGet, "/providers", "127.0.0.1:51000", req.host, req.header); !strings.Contains(w.Body.String(), `"dev_login":false`) {
			t.Errorf("providers through a local proxy (%s) = %s, want dev_login false", name, w.Body.String())
		}
	}
	for _, host := range []string{"127.0.0.1:9000", "[::1]:9000", "app.localhost:5173", "localhost"} {
		if w := callAs(http.MethodGet, "/providers", "127.0.0.1:51000", host, nil); !strings.Contains(w.Body.String(), `"dev_login":true`) {
			t.Errorf("providers for host %s = %s, want dev_login true", host, w.Body.String())
		}
	}
	for _, peer := range []string{"203.0.113.7:51000", "172.17.0.1:40000", "[2001:db8::1]:443", "garbage"} {
		if w := call(http.MethodPost, "/dev-login", peer); w.Code != http.StatusNotFound {
			t.Errorf("dev-login from %s = %d, want 404", peer, w.Code)
		}
		if w := call(http.MethodGet, "/providers", peer); !strings.Contains(w.Body.String(), `"dev_login":false`) {
			t.Errorf("providers from %s = %s, want dev_login false", peer, w.Body.String())
		}
	}
	for _, peer := range []string{"127.0.0.1:51000", "[::1]:51000"} {
		if w := call(http.MethodGet, "/providers", peer); !strings.Contains(w.Body.String(), `"dev_login":true`) {
			t.Errorf("providers from %s = %s, want dev_login true", peer, w.Body.String())
		}
	}

	// A public BASE_URL closes it for everyone, and says so.
	h.Config.BaseURL = "https://localhost.licensing.example.com"
	if w := call(http.MethodGet, "/providers", "127.0.0.1:51000"); !strings.Contains(w.Body.String(), `"dev_login":false`) {
		t.Errorf("providers with a public BASE_URL = %s, want dev_login false", w.Body.String())
	}
	if w := call(http.MethodPost, "/dev-login", "127.0.0.1:51000"); w.Code != http.StatusNotFound {
		t.Errorf("dev-login with a public BASE_URL = %d, want 404", w.Code)
	}
}
