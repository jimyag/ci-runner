//go:build development

package server

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

const uiAssetsDevelopment = true

func (s *Server) serveUIAsset(w http.ResponseWriter, r *http.Request, _ string) bool {
	origin, err := uiDevServerURL()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.Header.Set("X-Forwarded-Host", req.Host)
			req.Header.Set("X-Origin-Host", origin.Host)
			req.URL.Scheme = origin.Scheme
			req.URL.Host = origin.Host
			req.Host = origin.Host
		},
	}
	proxy.ServeHTTP(w, r)
	return true
}

func uiDevServerURL() (*url.URL, error) {
	raw := os.Getenv("RUNNERD_VITE_DEV_SERVER_URL")
	if raw == "" {
		port := os.Getenv("RUNNERD_VITE_PORT")
		if port == "" {
			port = "5173"
		}
		raw = fmt.Sprintf("http://127.0.0.1:%s", port)
	}
	return url.Parse(raw)
}
