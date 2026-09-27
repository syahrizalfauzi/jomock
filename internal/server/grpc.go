package server

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// gRPC status codes the mock itself returns (stubs set their own).
const (
	grpcOK            = 0
	grpcNotFound      = 5
	grpcUnimplemented = 12
	grpcInternal      = 13
)

// isGRPC reports whether the request is a gRPC call. gRPC-Web is a different
// framing over HTTP/1 and is not handled here.
func isGRPC(r *http.Request) bool {
	if r.ProtoMajor != 2 {
		return false
	}
	ct := r.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "application/grpc") && !strings.Contains(ct, "-web")
}

// readFrames splits a gRPC body into its length-prefixed messages.
// Frame layout: 1 byte compression flag, 4 byte big-endian length, payload.
func readFrames(b []byte) ([][]byte, error) {
	var out [][]byte
	for len(b) >= 5 {
		if b[0] != 0 {
			return nil, fmt.Errorf("compressed gRPC frames are not supported")
		}
		n := int(binary.BigEndian.Uint32(b[1:5]))
		if n < 0 || len(b) < 5+n {
			return nil, fmt.Errorf("truncated gRPC frame: want %d bytes, have %d", n, len(b)-5)
		}
		out = append(out, b[5:5+n])
		b = b[5+n:]
	}
	if len(b) != 0 {
		return nil, fmt.Errorf("trailing bytes after gRPC frames")
	}
	return out, nil
}

// frame wraps a message in the gRPC length-prefixed envelope.
func frame(msg []byte) []byte {
	out := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(msg)))
	copy(out[5:], msg)
	return out
}

// encodeGrpcMessage percent-encodes a trailer value as the gRPC spec requires.
func encodeGrpcMessage(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e || c == '%' {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func (m *mock) serveGRPC(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	raw, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	_ = r.Body.Close()

	payload, status, message, token := m.grpcResponse(r, raw)

	// Headers must be complete before WriteHeader; trailers are declared here
	// and assigned after the body, which is how Go emits them. Add, not Set:
	// Set replaces the trailer list, dropping every entry but the last.
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Add("Trailer", "Grpc-Status")
	w.Header().Add("Trailer", "Grpc-Message")
	w.WriteHeader(http.StatusOK)
	if status == grpcOK {
		_, _ = w.Write(frame(payload))
	}
	w.Header().Set("Grpc-Status", strconv.Itoa(status))
	w.Header().Set("Grpc-Message", encodeGrpcMessage(message))

	dur := time.Since(start)
	m.st.Complete(token, status, dur)
	m.lg.Info("request",
		"proto", "grpc",
		"url", r.URL.Path,
		"status", status,
		"ms", dur.Milliseconds(),
	)
}

// grpcResponse resolves the answer. It always records a journal entry, so a
// failed call is visible even when no stub matched.
func (m *mock) grpcResponse(r *http.Request, raw []byte) (payload []byte, status int, message string, token int) {
	reg := m.schema.Get()
	frames, frameErr := readFrames(raw)
	md, mdErr := reg.Lookup(r.URL.Path)

	// The matcher sees ProtoJSON, so equalJson works exactly like it does for HTTP.
	var matchBody []byte
	if len(frames) > 0 {
		switch {
		case frameErr != nil:
		case mdErr != nil:
			matchBody = frames[0]
		default:
			decoded, err := reg.DecodeMessage(md, frames[0])
			if err != nil {
				frameErr = err
			} else {
				matchBody = decoded
			}
		}
	}

	matched, token := m.st.Match(r, matchBody, "grpc")

	switch {
	case frameErr != nil:
		return nil, grpcInternal, frameErr.Error(), token
	case mdErr != nil:
		return nil, grpcUnimplemented, mdErr.Error(), token
	case matched.ID == "":
		return nil, grpcNotFound, "no matching stub for " + r.URL.Path, token
	case matched.Response.GrpcStatus != 0:
		return nil, matched.Response.GrpcStatus, matched.Response.GrpcMessage, token
	case matched.Response.JSONBody != nil:
		out, err := reg.EncodeMessage(md, matched.Response.JSONBody)
		if err != nil {
			return nil, grpcInternal, err.Error(), token
		}
		return out, grpcOK, matched.Response.GrpcMessage, token
	default:
		return []byte(matched.Response.Body), grpcOK, matched.Response.GrpcMessage, token
	}
}
