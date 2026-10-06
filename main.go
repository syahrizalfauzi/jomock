// Command jomock is a standalone mock server: WireMock-style stubs defined as
// JSON, served over HTTP and gRPC, managed from an admin API and an embedded UI.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/syahrizalfauzi/jomock/internal/schema"
	"github.com/syahrizalfauzi/jomock/internal/server"
	"github.com/syahrizalfauzi/jomock/internal/store"
)

//go:embed web
var webFS embed.FS

func main() {
	mockAddr := flag.String("mock-addr", ":8081", "address for the mock server under test")
	adminAddr := flag.String("admin-addr", ":8080", "address for the admin API and UI")
	stubPath := flag.String("stubs", "stubs.json", "path to the stub file (empty disables persistence)")
	protoPath := flag.String("proto", "", "protobuf FileDescriptorSet for gRPC mocking (empty disables gRPC)")
	protosPath := flag.String("protos", "protos.json", "where .proto files imported in the UI are cached (empty disables)")
	journalSize := flag.Int("journal-size", 1000, "number of requests kept in the journal")
	flag.Parse()

	lg := slog.New(slog.NewTextHandler(os.Stdout, nil))

	st := store.New(*stubPath, *journalSize)
	if err := st.Load(); err != nil {
		lg.Error("load stubs", "path", *stubPath, "err", err)
		os.Exit(1)
	}

	reg, err := schema.Load(*protoPath)
	if err != nil {
		lg.Error("load proto descriptors", "path", *protoPath, "err", err)
		os.Exit(1)
	}

	web, err := fs.Sub(webFS, "web")
	if err != nil {
		lg.Error("embedded web assets", "err", err)
		os.Exit(1)
	}

	// The registry is swappable so the UI can import .proto files at runtime, and
	// the sources are cached so a restart does not need a re-import.
	regs := schema.NewSet(reg, *protosPath)
	if err := regs.LoadCache(); err != nil {
		lg.Warn("cached protos did not compile", "path", *protosPath, "err", err)
	}

	mockSrv := &http.Server{
		Addr:      *mockAddr,
		Handler:   server.NewMock(st, regs, lg),
		Protocols: h2cAndHTTP1(),
	}
	adminSrv := &http.Server{Addr: *adminAddr, Handler: server.NewAdmin(st, regs, web, lg)}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 2)
	go serve(adminSrv, errc)
	go serve(mockSrv, errc)
	lg.Info("jomock up",
		"mock", *mockAddr,
		"admin", *adminAddr,
		"stubs", *stubPath,
		"loaded", len(st.List()),
		"rpcs", len(reg.Methods()),
	)

	select {
	case <-ctx.Done():
		lg.Info("shutting down")
	case err := <-errc:
		lg.Error("server failed", "err", err)
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = adminSrv.Shutdown(shutdown)
	_ = mockSrv.Shutdown(shutdown)
}

// h2cAndHTTP1 lets the mock port speak HTTP/1.1 and unencrypted HTTP/2 on the
// same address, which is what gRPC clients need when they are not using TLS.
func h2cAndHTTP1() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

func serve(srv *http.Server, errc chan<- error) {
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errc <- err
	}
}
