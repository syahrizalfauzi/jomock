package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/syahrizalfauzi/jomock/internal/schema"
	"github.com/syahrizalfauzi/jomock/internal/store"
	"github.com/syahrizalfauzi/jomock/internal/stub"
)

const (
	uploadGoodProto = `
syntax = "proto3";
package echo.v1;
message EchoRequest { string message = 1; }
message EchoReply { string message = 1; }
service Echo { rpc Say(EchoRequest) returns (EchoReply); }
`

	uploadBrokenProto = `
syntax = "proto3";
package broken.v1;
message Bad { this is not proto }
`
)

// TestGRPCProtoUpload covers the UI path: no protoc, no descriptor file, just
// .proto sources posted to the admin API, including a folder that does not compile.
func TestGRPCProtoUpload(t *testing.T) {
	st := store.New(filepath.Join(t.TempDir(), "stubs.json"), 50)
	set := schema.NewSet(nil, filepath.Join(t.TempDir(), "protos.json"))
	base, client := newH2C(t, st, set)

	admin := httptest.NewServer(NewAdmin(st, set, fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>ui</html>")},
	}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer admin.Close()

	if _, status, _ := grpcCall(t, client, base+"/echo.v1.Echo/Say", echoRequest("hi")); status != "12" {
		t.Fatalf("before upload grpc-status = %q, want 12", status)
	}

	payload, err := json.Marshal(map[string]any{"files": map[string]string{
		"proto/echo-service/echo.proto":     uploadGoodProto,
		"proto/broken-service/broken.proto": uploadBrokenProto,
	}})
	if err != nil {
		t.Fatalf("marshal upload: %v", err)
	}

	res, err := http.Post(admin.URL+"/__admin/grpc/protos", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("upload status %d: %s", res.StatusCode, body)
	}

	var summary schema.Summary
	if err := json.NewDecoder(res.Body).Decode(&summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if len(summary.Methods) != 1 || summary.Methods[0] != "/echo.v1.Echo/Say" {
		t.Fatalf("methods = %v", summary.Methods)
	}
	if len(summary.Services) != 1 || summary.Services[0] != "echo.v1.Echo" {
		t.Fatalf("services = %v", summary.Services)
	}
	if len(summary.Warnings) != 1 || !strings.Contains(summary.Warnings[0], "broken-service") {
		t.Fatalf("warnings = %v", summary.Warnings)
	}
	if summary.Source != "upload" {
		t.Fatalf("source = %q, want upload", summary.Source)
	}

	if _, err := st.Add(stub.Stub{
		Type:     stub.TypeGRPC,
		Request:  stub.RequestPattern{Path: "/echo.v1.Echo/Say"},
		Response: stub.ResponseDef{JSONBody: map[string]any{"message": "uploaded"}},
	}); err != nil {
		t.Fatalf("add stub: %v", err)
	}

	frames, status, _ := grpcCall(t, client, base+"/echo.v1.Echo/Say", echoRequest("hi"))
	if status != "0" {
		t.Fatalf("after upload grpc-status = %q, want 0", status)
	}
	want := append([]byte{0x0a, 0x08}, []byte("uploaded")...)
	if len(frames) != 1 || !bytes.Equal(frames[0], want) {
		t.Fatalf("reply = %q, want %q", frames, want)
	}

	// the schema endpoint reports the same picture after a page refresh
	res2, err := http.Get(admin.URL + "/__admin/grpc/schema")
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	defer res2.Body.Close()
	var again schema.Summary
	if err := json.NewDecoder(res2.Body).Decode(&again); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	if len(again.Methods) != 1 || len(again.Warnings) != 1 {
		t.Fatalf("schema endpoint = %+v", again)
	}
}

func TestGRPCProtoUploadRejectsEmpty(t *testing.T) {
	st := store.New("", 10)
	set := schema.NewSet(nil, "")
	admin := httptest.NewServer(NewAdmin(st, set, fstest.MapFS{}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer admin.Close()

	res, err := http.Post(admin.URL+"/__admin/grpc/protos", "application/json", strings.NewReader(`{"files":{}}`))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty upload status = %d, want 400", res.StatusCode)
	}
}
