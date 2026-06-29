//go:build development

package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHandleAdminServesUIDevServerInDevelopment(t *testing.T) {
	var devServerHost string
	devServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/" {
			t.Fatalf("expected proxy to preserve admin path, got %q", r.URL.Path)
		}
		if r.Host != devServerHost {
			t.Fatalf("expected proxy to set request host to Vite host, got host=%q want %q", r.Host, devServerHost)
		}
		if r.Header.Get("X-Forwarded-Host") != "runnerd.test" {
			t.Fatalf("expected original host to be forwarded, got %q", r.Header.Get("X-Forwarded-Host"))
		}
		if r.Header.Get("X-Origin-Host") != devServerHost {
			t.Fatalf("expected Vite host to be recorded, got %q want %q", r.Header.Get("X-Origin-Host"), devServerHost)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>dev admin shell</html>"))
	}))
	t.Cleanup(devServer.Close)
	devServerURL, err := url.Parse(devServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	devServerHost = devServerURL.Host
	t.Setenv("RUNNERD_VITE_DEV_SERVER_URL", devServer.URL)

	req := httptest.NewRequest(http.MethodGet, "http://runnerd.test/admin/", nil)
	rec := httptest.NewRecorder()

	(&Server{}).handleAdmin(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected development admin proxy to return 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "dev admin shell") {
		t.Fatalf("expected Vite dev server response, got %q", rec.Body.String())
	}
}
