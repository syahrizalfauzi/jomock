package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/syahrizalfauzi/jomock/internal/schema"
	"github.com/syahrizalfauzi/jomock/internal/store"
	"github.com/syahrizalfauzi/jomock/internal/stub"
)

type mock struct {
	st     *store.Store
	schema *schema.Set
	lg     *slog.Logger
}

// NewMock returns the handler for the mock port. It serves HTTP/1.1 and h2c on
// the same address, and answers gRPC calls when descriptors are loaded.
func NewMock(st *store.Store, reg *schema.Set, lg *slog.Logger) http.Handler {
	m := &mock{st: st, schema: reg, lg: lg}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isGRPC(r) {
			m.serveGRPC(w, r)
			return
		}
		m.serveHTTP(w, r)
	})
}

func (m *mock) serveHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqBody, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	_ = r.Body.Close()

	matched, token := m.st.Match(r, reqBody, "http")
	status := http.StatusNotFound
	if matched.ID == "" {
		writeJSON(w, status, map[string]string{
			"error":  "no matching stub",
			"method": r.Method,
			"path":   r.URL.Path,
		})
	} else {
		status = matched.Response.Status
		if status == 0 {
			status = http.StatusOK
		}
		for k, v := range matched.Response.Headers {
			w.Header().Set(k, v)
		}
		body, isJSON := responseBody(matched.Response)
		if isJSON && w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		if d := time.Duration(matched.Response.DelayMs) * time.Millisecond; d > 0 {
			time.Sleep(d)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}

	dur := time.Since(start)
	m.st.Complete(token, status, dur)
	m.lg.Info("request",
		"proto", "http",
		"method", r.Method,
		"url", r.URL.RequestURI(),
		"status", status,
		"stub", matched.ID,
		"ms", dur.Milliseconds(),
	)
}

// responseBody resolves the bytes to serve. JSONBody wins over Body; the bool
// reports whether the body is JSON so the caller can set Content-Type.
func responseBody(r stub.ResponseDef) ([]byte, bool) {
	if r.JSONBody != nil {
		if b, err := json.Marshal(r.JSONBody); err == nil {
			return b, true
		}
	}
	return []byte(r.Body), false
}
