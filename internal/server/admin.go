// Package server wires the store into an admin API + UI and a mock HTTP endpoint.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/syahrizalfauzi/jomock/internal/schema"
	"github.com/syahrizalfauzi/jomock/internal/store"
	"github.com/syahrizalfauzi/jomock/internal/stub"
)

const maxBodyBytes = 10 << 20

type admin struct {
	st     *store.Store
	schema *schema.Set
	lg     *slog.Logger
}

// NewAdmin returns the handler for the admin API and the embedded UI.
func NewAdmin(st *store.Store, reg *schema.Set, web fs.FS, lg *slog.Logger) http.Handler {
	a := &admin{st: st, schema: reg, lg: lg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__admin/health", a.health)
	mux.HandleFunc("GET /__admin/mappings", a.listMappings)
	mux.HandleFunc("POST /__admin/mappings", a.createMapping)
	mux.HandleFunc("PUT /__admin/mappings", a.replaceMappings)
	mux.HandleFunc("GET /__admin/mappings/{id}", a.getMapping)
	mux.HandleFunc("PUT /__admin/mappings/{id}", a.putMapping)
	mux.HandleFunc("DELETE /__admin/mappings/{id}", a.deleteMapping)
	mux.HandleFunc("POST /__admin/mappings/{id}/move", a.moveMapping)
	mux.HandleFunc("GET /__admin/requests", a.listRequests)
	mux.HandleFunc("POST /__admin/verify", a.verify)
	mux.HandleFunc("GET /__admin/grpc/methods", a.grpcMethods)
	mux.HandleFunc("GET /__admin/grpc/schema", a.grpcSchema)
	mux.HandleFunc("POST /__admin/grpc/protos", a.uploadProtos)
	mux.Handle("GET /", http.FileServerFS(web))
	return mux
}

func (a *admin) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "stubs": len(a.st.List())})
}

func (a *admin) listMappings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.st.List())
}

func (a *admin) createMapping(w http.ResponseWriter, r *http.Request) {
	var s stub.Stub
	if !decode(w, r, &s) {
		return
	}
	created, err := a.st.Add(s)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// replaceMappings swaps the whole list; the UI Open button uses it.
func (a *admin) replaceMappings(w http.ResponseWriter, r *http.Request) {
	var stubs []stub.Stub
	if !decode(w, r, &stubs) {
		return
	}
	next, err := a.st.Replace(stubs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, next)
}

func (a *admin) getMapping(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, s := range a.st.List() {
		if s.ID == id {
			writeJSON(w, http.StatusOK, s)
			return
		}
	}
	writeError(w, http.StatusNotFound, errors.New("no stub with id "+id))
}

func (a *admin) putMapping(w http.ResponseWriter, r *http.Request) {
	var s stub.Stub
	if !decode(w, r, &s) {
		return
	}
	updated, ok, err := a.st.Update(r.PathValue("id"), s)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("no stub with id "+r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (a *admin) deleteMapping(w http.ResponseWriter, r *http.Request) {
	if !a.st.Delete(r.PathValue("id")) {
		writeError(w, http.StatusNotFound, errors.New("no stub with id "+r.PathValue("id")))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *admin) moveMapping(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Direction string `json:"direction"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Direction != "up" && body.Direction != "down" {
		writeError(w, http.StatusBadRequest, errors.New("direction must be up or down"))
		return
	}
	if !a.st.Move(r.PathValue("id"), body.Direction) {
		writeError(w, http.StatusNotFound, errors.New("no stub with id "+r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, a.st.List())
}

func (a *admin) listRequests(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, a.st.Journal(limit))
}

func (a *admin) verify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		StubID        string `json:"stubId"`
		ExpectedCount *int   `json:"expectedCount"`
	}
	if !decode(w, r, &body) {
		return
	}
	n, passed := a.st.Verify(body.StubID, body.ExpectedCount)
	writeJSON(w, http.StatusOK, map[string]any{"stubId": body.StubID, "matched": n, "passed": passed})
}

// grpcMethods lists the rpcs available for stubbing, as "/pkg.Service/Method".
func (a *admin) grpcMethods(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.schema.Methods())
}

// grpcSchema reports what is currently loaded, including per-folder warnings.
func (a *admin) grpcSchema(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.schema.Summary())
}

// uploadProtos compiles .proto sources sent by the UI, so protoc is not needed.
// Files are keyed by their path relative to the folder the user picked.
func (a *admin) uploadProtos(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Files map[string]string `json:"files"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Files) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("no files in the upload"))
		return
	}

	sources := make(map[string][]byte, len(body.Files))
	for path, content := range body.Files {
		sources[path] = []byte(content)
	}

	summary, err := a.schema.Import(sources)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	a.lg.Info("grpc schema imported",
		"files", len(body.Files),
		"services", len(summary.Services),
		"methods", len(summary.Methods),
		"warnings", len(summary.Warnings),
	)
	writeJSON(w, http.StatusOK, summary)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
