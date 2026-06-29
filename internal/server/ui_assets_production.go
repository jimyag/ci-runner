//go:build !development

package server

import (
	"embed"
	"mime"
	"net/http"
	"path"
)

const uiAssetsDevelopment = false

//go:embed ui/*
var uiAssets embed.FS

func (s *Server) serveUIAsset(w http.ResponseWriter, _ *http.Request, name string) bool {
	data, err := uiAssets.ReadFile("ui" + name)
	if err != nil {
		if path.Ext(name) != "" {
			return false
		}
		data, err = uiAssets.ReadFile("ui/index.html")
		if err != nil {
			return false
		}
		name = "/index.html"
	}
	if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if w.Header().Get("Content-Type") == "" {
		if contentType := fallbackContentType(path.Ext(name)); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
	return true
}

func fallbackContentType(ext string) string {
	switch ext {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".json":
		return "application/json"
	default:
		return ""
	}
}
