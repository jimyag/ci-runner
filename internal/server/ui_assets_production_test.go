//go:build !development

package server

import "testing"

func TestFallbackContentTypeForWebAssets(t *testing.T) {
	tests := map[string]string{
		".html": "text/html; charset=utf-8",
		".js":   "text/javascript; charset=utf-8",
		".mjs":  "text/javascript; charset=utf-8",
		".css":  "text/css; charset=utf-8",
		".svg":  "image/svg+xml",
		".png":  "image/png",
		".json": "application/json",
	}
	for ext, want := range tests {
		if got := fallbackContentType(ext); got != want {
			t.Fatalf("fallbackContentType(%q) = %q, want %q", ext, got, want)
		}
	}
	if got := fallbackContentType(".txt"); got != "" {
		t.Fatalf("fallbackContentType for unknown extension = %q, want empty", got)
	}
}
