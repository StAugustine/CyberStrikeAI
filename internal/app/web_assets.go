package app

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
)

type appWebAssets struct {
	templates *template.Template
	static    http.FileSystem
}

func prepareWebAssets(source fs.FS) (*appWebAssets, error) {
	if source == nil {
		return nil, fmt.Errorf("web filesystem is required")
	}
	templates, err := template.ParseFS(source, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse web templates: %w", err)
	}
	staticFiles, err := fs.Sub(source, "static")
	if err != nil {
		return nil, fmt.Errorf("open static web assets: %w", err)
	}
	return &appWebAssets{templates: templates, static: http.FS(staticFiles)}, nil
}
