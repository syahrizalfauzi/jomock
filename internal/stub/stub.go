// Package stub defines the WireMock-shaped stub document.
package stub

import (
	"fmt"
	"regexp"
)

// Protocol values for Stub.Type.
const (
	TypeHTTP = "http"
	TypeGRPC = "grpc"
)

// Stub is one mapping: a request pattern and the response to serve.
type Stub struct {
	ID       string         `json:"id"`
	Type     string         `json:"type,omitempty"` // "http" (the default) or "grpc"
	Request  RequestPattern `json:"request"`
	Response ResponseDef    `json:"response"`
}

// Protocol returns the stub's protocol. An empty type means HTTP so that stub
// files written before the field existed keep matching.
func (s Stub) Protocol() string {
	if s.Type == TypeGRPC {
		return TypeGRPC
	}
	return TypeHTTP
}

// RequestPattern describes what a request must look like. Empty fields match anything.
// Query and Headers are subset matches: declared keys must match, extras are ignored.
type RequestPattern struct {
	Method      string            `json:"method,omitempty"`
	Path        string            `json:"path,omitempty"`
	PathPattern string            `json:"pathPattern,omitempty"`
	Query       map[string]string `json:"query,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        *BodyMatch        `json:"body,omitempty"`
}

// BodyMatch is the (optional) body matcher. At most one of the three is normally set.
type BodyMatch struct {
	Equal     string `json:"equal,omitempty"`
	Contains  string `json:"contains,omitempty"`
	EqualJSON any    `json:"equalJson,omitempty"`
}

// ResponseDef is what the mock server writes back.
type ResponseDef struct {
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	// JSONBody is marshalled back to JSON when the response is served, so raw
	// JSON can be pasted straight into the editor with no escaping. Takes
	// precedence over Body.
	JSONBody any `json:"jsonBody,omitempty"`
	DelayMs  int `json:"delayMs,omitempty"`
	// GrpcStatus and GrpcMessage are only used for gRPC calls. Status 0 is OK;
	// any other code is returned as an error and no message is sent.
	GrpcStatus  int    `json:"grpcStatus,omitempty"`
	GrpcMessage string `json:"grpcMessage,omitempty"`
}

// Validate rejects stubs that can never match or can never be served.
func (s Stub) Validate() error {
	switch s.Type {
	case "", TypeHTTP, TypeGRPC:
	default:
		return fmt.Errorf("type: %q is neither %q nor %q", s.Type, TypeHTTP, TypeGRPC)
	}
	if s.Request.PathPattern != "" {
		if _, err := regexp.Compile(s.Request.PathPattern); err != nil {
			return fmt.Errorf("request.pathPattern: %w", err)
		}
	}
	if s.Response.Status != 0 && (s.Response.Status < 100 || s.Response.Status > 599) {
		return fmt.Errorf("response.status: %d out of range 100-599", s.Response.Status)
	}
	return nil
}
