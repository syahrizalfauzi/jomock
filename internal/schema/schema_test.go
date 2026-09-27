package schema

import (
	"path/filepath"
	"strings"
	"testing"
)

const (
	goodProto = `
syntax = "proto3";
package good.v1;
message M { string s = 1; }
service S { rpc Do(M) returns (M); }
`

	goodImport = `
syntax = "proto3";
package good.v1;
import "good-service/a.proto";
message N { M m = 1; }
`

	brokenProto = `
syntax = "proto3";
package bad.v1;
message B { nope }
`

	dupProto = `
syntax = "proto3";
package dup.v1;
message Same { string s = 1; }
service S { rpc Do(Same) returns (Same); }
`
)

func TestCompileGroupsFoldersAndReportsWarnings(t *testing.T) {
	reg, err := Compile(map[string][]byte{
		"proto/good-service/a.proto":  []byte(goodProto),
		"proto/good-service/b.proto":  []byte(goodImport),
		"proto/bad-service/bad.proto": []byte(brokenProto),
		"proto/notes.txt":             []byte("not a proto"),
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if got := strings.Join(reg.Methods(), ","); got != "/good.v1.S/Do" {
		t.Fatalf("Methods() = %q", got)
	}
	if len(reg.warnings) != 1 || !strings.Contains(reg.warnings[0], "bad-service") {
		t.Fatalf("warnings = %v", reg.warnings)
	}
	if _, err := reg.Lookup("/good.v1.S/Do"); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if _, err := reg.Lookup("/good.v1.S/Nope"); err == nil {
		t.Fatal("Lookup should fail for an unknown method")
	}
}

// Real proto trees define the same symbol in two folders. That must not cost you
// the whole import, which is why folders stay separate lookup spaces.
func TestCompileToleratesCrossFolderDuplicateSymbols(t *testing.T) {
	reg, err := Compile(map[string][]byte{
		"proto/alpha/s.proto": []byte(dupProto),
		"proto/beta/s.proto":  []byte(dupProto),
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(reg.warnings) != 0 {
		t.Fatalf("warnings = %v, want none", reg.warnings)
	}
	if got := strings.Join(reg.Methods(), ","); got != "/dup.v1.S/Do" {
		t.Fatalf("Methods() = %q", got)
	}
}

// Importing in the UI should survive a restart: that is the whole point of the cache.
func TestSetCachesImportedSources(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "protos.json")

	set := NewSet(nil, cache)
	if _, err := set.Import(map[string][]byte{"proto/good-service/a.proto": []byte(goodProto)}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if got := strings.Join(set.Methods(), ","); got != "/good.v1.S/Do" {
		t.Fatalf("after import Methods() = %q", got)
	}

	// a fresh process
	restarted := NewSet(nil, cache)
	if err := restarted.LoadCache(); err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if got := strings.Join(restarted.Methods(), ","); got != "/good.v1.S/Do" {
		t.Fatalf("after restart Methods() = %q", got)
	}
}

func TestSetCacheIsOptional(t *testing.T) {
	// no cache path: import still works, nothing is written
	set := NewSet(nil, "")
	if _, err := set.Import(map[string][]byte{"proto/a/x.proto": []byte(goodProto)}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if err := NewSet(nil, "").LoadCache(); err != nil {
		t.Fatalf("LoadCache with no path: %v", err)
	}
	// missing cache file is not an error either
	if err := NewSet(nil, filepath.Join(t.TempDir(), "nope.json")).LoadCache(); err != nil {
		t.Fatalf("LoadCache with no file: %v", err)
	}
}

func TestCompileWithoutProtos(t *testing.T) {
	if _, err := Compile(map[string][]byte{"readme.md": []byte("hi")}); err == nil {
		t.Fatal("expected an error when the upload has no .proto files")
	}
}

func TestNilSetIsSafe(t *testing.T) {
	var set *Set
	if got := set.Methods(); len(got) != 0 {
		t.Fatalf("nil Set.Methods() = %v", got)
	}
	if _, err := set.Get().Lookup("/a.B/C"); err == nil {
		t.Fatal("looking up against no descriptors should fail")
	}
	if got := set.Summary(); len(got.Methods) != 0 || len(got.Warnings) != 0 {
		t.Fatalf("nil Set.Summary() = %+v", got)
	}
}
