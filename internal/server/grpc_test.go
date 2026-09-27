package server

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/syahrizalfauzi/jomock/internal/schema"
	"github.com/syahrizalfauzi/jomock/internal/store"
	"github.com/syahrizalfauzi/jomock/internal/stub"
)

func strField(name string, num int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:     proto.String(name),
		Number:   proto.Int32(num),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		JsonName: proto.String(name),
	}
}

// testRegistry writes a descriptor set for echo.v1.Echo by hand, so the tests
// do not need protoc on the machine.
func testRegistry(t *testing.T) *schema.Set {
	t.Helper()
	msg := func(name string) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{Name: proto.String(name), Field: []*descriptorpb.FieldDescriptorProto{strField("message", 1)}}
	}
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name:        proto.String("echo.proto"),
		Package:     proto.String("echo.v1"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{msg("EchoRequest"), msg("EchoReply")},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("Echo"),
			Method: []*descriptorpb.MethodDescriptorProto{
				{Name: proto.String("Say"), InputType: proto.String(".echo.v1.EchoRequest"), OutputType: proto.String(".echo.v1.EchoReply")},
				{Name: proto.String("Fail"), InputType: proto.String(".echo.v1.EchoRequest"), OutputType: proto.String(".echo.v1.EchoReply")},
			},
		}},
	}}}

	raw, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshal descriptor set: %v", err)
	}
	path := filepath.Join(t.TempDir(), "desc.bin")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write descriptor set: %v", err)
	}
	reg, err := schema.Load(path)
	if err != nil {
		t.Fatalf("load descriptor set: %v", err)
	}
	return schema.NewSet(reg, "")
}

// newH2C starts the mock handler on h2c and returns an HTTP/2-prior-knowledge client.
func newH2C(t *testing.T, st *store.Store, reg *schema.Set) (string, *http.Client) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serve := new(http.Protocols)
	serve.SetHTTP1(true)
	serve.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: NewMock(st, reg, slog.New(slog.NewTextHandler(io.Discard, nil))), Protocols: serve}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })

	want := new(http.Protocols)
	want.SetUnencryptedHTTP2(true)
	return "http://" + ln.Addr().String(), &http.Client{Transport: &http.Transport{Protocols: want}}
}

// grpcCall posts one gRPC message and returns the reply frames and trailers.
func grpcCall(t *testing.T, c *http.Client, url string, msg []byte) (frames [][]byte, status, message string) {
	t.Helper()
	req, err := http.NewRequest("POST", url, bytes.NewReader(frame(msg)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")

	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("grpc call: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	frames, err = readFrames(body)
	if err != nil {
		t.Fatalf("readFrames(%q): %v", body, err)
	}
	return frames, res.Trailer.Get("Grpc-Status"), res.Trailer.Get("Grpc-Message")
}

// echoRequest is the protobuf encoding of echo.v1.EchoRequest{message: v}.
func echoRequest(v string) []byte {
	out := []byte{0x0a, byte(len(v))}
	return append(out, v...)
}

func TestGRPCMock(t *testing.T) {
	st := store.New(filepath.Join(t.TempDir(), "stubs.json"), 50)
	reg := testRegistry(t)
	base, client := newH2C(t, st, reg)

	if won := strings.Join(reg.Methods(), ","); won != "/echo.v1.Echo/Fail,/echo.v1.Echo/Say" {
		t.Fatalf("Methods() = %q", won)
	}

	if _, err := st.Add(stub.Stub{
		Type:     stub.TypeGRPC,
		Request:  stub.RequestPattern{Path: "/echo.v1.Echo/Say", Body: &stub.BodyMatch{EqualJSON: map[string]any{"message": "hi"}}},
		Response: stub.ResponseDef{JSONBody: map[string]any{"message": "hello"}},
	}); err != nil {
		t.Fatalf("add stub: %v", err)
	}
	if _, err := st.Add(stub.Stub{
		Type:     stub.TypeGRPC,
		Request:  stub.RequestPattern{Path: "/echo.v1.Echo/Fail"},
		Response: stub.ResponseDef{GrpcStatus: 3, GrpcMessage: "boom"},
	}); err != nil {
		t.Fatalf("add stub: %v", err)
	}

	// matching call: the JSON body is encoded as protobuf
	frames, status, message := grpcCall(t, client, base+"/echo.v1.Echo/Say", echoRequest("hi"))
	if status != "0" {
		t.Fatalf("grpc-status = %q, want 0", status)
	}
	want := []byte{0x0a, 0x05, 'h', 'e', 'l', 'l', 'o'}
	if len(frames) != 1 || !bytes.Equal(frames[0], want) {
		t.Fatalf("reply frames = %q, want one frame %q", frames, want)
	}

	// body matcher sees ProtoJSON, so a different message does not match
	if _, status, _ = grpcCall(t, client, base+"/echo.v1.Echo/Say", echoRequest("bye")); status != "5" {
		t.Fatalf("unmatched body grpc-status = %q, want 5", status)
	}

	// unknown method in a known service
	if _, status, message = grpcCall(t, client, base+"/echo.v1.Echo/Nope", echoRequest("hi")); status != "12" {
		t.Fatalf("unknown method grpc-status = %q, want 12", status)
	}
	if !strings.Contains(message, "no method Nope") {
		t.Fatalf("unknown method message = %q", message)
	}

	// stub-declared error status: no message frame, trailer carries the text
	frames, status, message = grpcCall(t, client, base+"/echo.v1.Echo/Fail", echoRequest("hi"))
	if status != "3" || message != "boom" {
		t.Fatalf("error stub status=%q message=%q, want 3/boom", status, message)
	}
	if len(frames) != 0 {
		t.Fatalf("error response carried %d frames, want none", len(frames))
	}

	// HTTP traffic still works on the same port
	res, err := http.Get(base + "/nothing")
	if err != nil {
		t.Fatalf("http get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("http status = %d, want 404", res.StatusCode)
	}

	// journal records both protocols
	var sawGRPC, sawHTTP bool
	for _, e := range st.Journal(10) {
		switch e.Protocol {
		case "grpc":
			sawGRPC = true
			if e.URL != "/echo.v1.Echo/Fail" && e.Status == 3 {
				t.Fatalf("grpc journal entry = %+v", e)
			}
		case "http":
			sawHTTP = true
		}
	}
	if !sawGRPC || !sawHTTP {
		t.Fatalf("journal protocols: grpc=%v http=%v", sawGRPC, sawHTTP)
	}
}

// A stub answers its own protocol only: sharing a path across HTTP and gRPC
// must not let one swallow the other.
func TestStubTypeIsolatesProtocols(t *testing.T) {
	st := store.New(filepath.Join(t.TempDir(), "stubs.json"), 50)
	set := testRegistry(t)
	base, client := newH2C(t, st, set)

	// an HTTP stub sitting on a real rpc path
	if _, err := st.Add(stub.Stub{
		Request:  stub.RequestPattern{Path: "/echo.v1.Echo/Say"},
		Response: stub.ResponseDef{JSONBody: map[string]any{"message": "http"}},
	}); err != nil {
		t.Fatalf("add stub: %v", err)
	}
	if _, status, _ := grpcCall(t, client, base+"/echo.v1.Echo/Say", echoRequest("hi")); status != "5" {
		t.Fatalf("HTTP stub answered a gRPC call: grpc-status = %q, want 5", status)
	}
	if code, _, body := do(t, "POST", base+"/echo.v1.Echo/Say", ""); code != 200 || !strings.Contains(body, "http") {
		t.Fatalf("HTTP stub: code=%d body=%q", code, body)
	}

	// a gRPC stub on a plain HTTP path
	if _, err := st.Add(stub.Stub{
		Type:     stub.TypeGRPC,
		Request:  stub.RequestPattern{Path: "/plain"},
		Response: stub.ResponseDef{JSONBody: map[string]any{"message": "grpc"}},
	}); err != nil {
		t.Fatalf("add stub: %v", err)
	}
	if code, _, _ := do(t, "GET", base+"/plain", ""); code != http.StatusNotFound {
		t.Fatalf("gRPC stub answered an HTTP request: %d", code)
	}
}

func TestGRPCWithoutDescriptors(t *testing.T) {
	st := store.New("", 10)
	base, client := newH2C(t, st, nil)

	_, status, message := grpcCall(t, client, base+"/echo.v1.Echo/Say", echoRequest("hi"))
	if status != "12" {
		t.Fatalf("grpc-status = %q, want 12", status)
	}
	if !strings.Contains(message, "no descriptor set loaded") {
		t.Fatalf("message = %q, want a hint about -proto", message)
	}
}
