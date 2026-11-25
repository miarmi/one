package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"lab/platform/appkit"
)

//go:embed web
var webFS embed.FS

const (
	maxNoteRunes = 280
	listLimit    = 100
	msgEmpty     = "Write something first."
	msgTooLong   = "Keep it under 280 characters."
)

type app struct {
	store   *store
	envText string
	version string
	page    *template.Template
	static  http.Handler
	icons   map[string][]byte
}

func newApp(st *store, appEnv, appVersion string) *app {
	envText := "development"
	switch {
	case appEnv == "prod":
		envText = "production"
	case appEnv != "":
		envText = "preview: " + appEnv
	}
	version := "dev"
	if appVersion != "" {
		version = appVersion
		if len(version) > 7 {
			version = version[:7]
		}
	}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	return &app{
		store:   st,
		envText: envText,
		version: version,
		page:    template.Must(template.ParseFS(webFS, "web/index.html")),
		static:  http.StripPrefix("/static/", http.FileServer(http.FS(sub))),
		icons:   makeIcons(),
	}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("POST /notes", a.addNote)
	mux.HandleFunc("POST /notes/{id}/delete", a.deleteNote)
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("GET /manifest.webmanifest", a.manifest)
	mux.HandleFunc("GET /apple-touch-icon.png", a.icon("apple-touch-icon.png"))
	mux.HandleFunc("GET /icon-192.png", a.icon("icon-192.png"))
	mux.HandleFunc("GET /icon-512.png", a.icon("icon-512.png"))
	mux.Handle("GET /static/style.css", a.static)
	return appkit.LogRequests(securityHeaders(http.NewCrossOriginProtection().Handler(mux)))
}

type pageData struct {
	EnvText string
	Version string
	Notes   []Note
	Message string
	Draft   string
}

func (a *app) render(w http.ResponseWriter, r *http.Request, status int, msg, draft string) {
	notes, err := a.store.List(r.Context(), listLimit)
	if err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	var buf bytes.Buffer
	if err := a.page.Execute(&buf, pageData{EnvText: a.envText, Version: a.version, Notes: notes, Message: msg, Draft: draft}); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (a *app) index(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "", "")
}

func (a *app) addNote(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	raw := r.PostForm.Get("text")
	text := strings.TrimSpace(raw)
	switch {
	case text == "":
		a.render(w, r, http.StatusUnprocessableEntity, msgEmpty, "")
		return
	case utf8.RuneCountInString(text) > maxNoteRunes || !utf8.ValidString(text):
		a.render(w, r, http.StatusUnprocessableEntity, msgTooLong, raw)
		return
	}
	if err := a.store.Add(r.Context(), text); err != nil {
		http.Error(w, "could not save the note", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *app) deleteNote(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.Error(w, "bad note id", http.StatusBadRequest)
		return
	}
	if err := a.store.Delete(r.Context(), id); err != nil {
		http.Error(w, "could not delete the note", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *app) healthz(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Ping(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok"))
}

func (a *app) manifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	json.NewEncoder(w).Encode(map[string]any{
		"name":             "Hello",
		"short_name":       "Hello",
		"start_url":        "/",
		"scope":            "/",
		"display":          "standalone",
		"background_color": "#f6f4ef",
		"theme_color":      iconColor,
		"icons": []map[string]string{
			{"src": "/icon-192.png", "sizes": "192x192", "type": "image/png"},
			{"src": "/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "any maskable"},
		},
	})
}

func (a *app) icon(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(a.icons[name])
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}
