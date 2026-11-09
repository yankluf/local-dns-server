// Package server exposes the JSON API and the web UI.
package server

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"github.com/yankluf/local-dns-server/internal/dnsmasq"
	"github.com/yankluf/local-dns-server/internal/docker"
	"github.com/yankluf/local-dns-server/internal/store"
)

//go:embed ui
var uiFiles embed.FS

// Server serves the API and UI.
type Server struct {
	Store     *store.Store
	DNSmasq   *dnsmasq.Manager
	Docker    *docker.Watcher // nil when discovery is off
	Upstreams []string
	Token     string
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/records", s.list)
	mux.HandleFunc("POST /api/records", s.auth(s.add))
	mux.HandleFunc("DELETE /api/records/{key}", s.auth(s.remove))
	mux.HandleFunc("GET /api/status", s.statusHandler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if s.DNSmasq != nil {
			if st := s.DNSmasq.Status(); st.Enabled && !st.Running {
				http.Error(w, "dnsmasq not running", http.StatusServiceUnavailable)
				return
			}
		}
		w.Write([]byte("ok\n"))
	})
	ui, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("GET /", http.FileServerFS(ui))
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
				writeError(w, http.StatusUnauthorized, errors.New("missing or wrong token (set LOCALDNS_TOKEN)"))
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"records": s.Store.List()})
}

func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		Comment string `json:"comment"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err := store.New(in.Name, in.Value, in.Comment)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err = s.Store.Add(rec)
	switch {
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, err)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Return the view so the caller sees any warning about the new record.
	for _, v := range s.Store.List() {
		if v.ID == rec.ID {
			writeJSON(w, http.StatusCreated, v)
			return
		}
	}
	writeJSON(w, http.StatusCreated, store.View{Record: rec, Active: true})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	rec, err := s.Store.Remove(r.PathValue("key"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, rec)
	}
}

func (s *Server) statusHandler(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"upstreams":     s.Upstreams,
		"auth_required": s.Token != "",
		"docker":        docker.Status{},
	}
	if s.DNSmasq != nil {
		out["dnsmasq"] = s.DNSmasq.Status()
	}
	if s.Docker != nil {
		out["docker"] = s.Docker.Status()
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
