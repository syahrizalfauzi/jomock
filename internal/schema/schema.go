// Package schema owns the protobuf descriptors behind gRPC mocking.
//
// Descriptors come from either a compiled FileDescriptorSet (-proto desc.bin) or
// .proto sources uploaded through the admin API, which are compiled in-process.
package schema

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// ErrNoDescriptors is returned when a gRPC call arrives but no descriptors are loaded.
var ErrNoDescriptors = errors.New("no descriptor set loaded; start jomock with -proto desc.bin, or import .proto files in the UI")

// Summary describes the loaded descriptors for the API and the UI.
type Summary struct {
	Services []string `json:"services"`
	Methods  []string `json:"methods"`
	Warnings []string `json:"warnings"`
	Source   string   `json:"source"`
}

// Registry resolves gRPC call paths to protobuf method descriptors.
//
// Compiled folders are kept as separate lookup spaces rather than merged into one
// set. Proto trees in the wild can define the same symbol in two folders; merging
// would reject the whole tree, while separate spaces simply never collide.
type Registry struct {
	groups   []*protoregistry.Files
	warnings []string
	source   string
	cache    sync.Map
}

// Set is a swappable registry: the UI can import new .proto files at runtime
// while requests are being served.
//
// cachePath, when set, keeps the imported .proto sources on disk so a restart
// does not mean importing them again. The file is the same shape as the upload
// body: {"path/to/file.proto": "<source>"}.
type Set struct {
	mu        sync.RWMutex
	reg       *Registry
	cachePath string
}

// NewSet wraps a registry (which may be nil when gRPC is not configured yet).
func NewSet(r *Registry, cachePath string) *Set {
	return &Set{reg: r, cachePath: cachePath}
}

// Import compiles .proto sources, swaps them in, and caches them for the next run.
func (s *Set) Import(sources map[string][]byte) (Summary, error) {
	reg, err := Compile(sources)
	if err != nil {
		return Summary{}, err
	}
	s.Replace(reg)
	summary := reg.Summary()

	if err := s.saveCache(sources); err != nil {
		summary.Warnings = append(summary.Warnings, "could not cache .proto sources: "+err.Error())
	}
	return summary, nil
}

// LoadCache restores sources cached by an earlier run. A missing cache is not an error.
func (s *Set) LoadCache() error {
	if s.cachePath == "" {
		return nil
	}
	raw, err := os.ReadFile(s.cachePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var cached map[string]string
	if err := json.Unmarshal(raw, &cached); err != nil {
		return fmt.Errorf("parse %s: %w", s.cachePath, err)
	}
	if len(cached) == 0 {
		return nil
	}

	sources := make(map[string][]byte, len(cached))
	for path, content := range cached {
		sources[path] = []byte(content)
	}
	reg, err := Compile(sources)
	if err != nil {
		return fmt.Errorf("%s: %w", s.cachePath, err)
	}
	s.Replace(reg)
	return nil
}

func (s *Set) saveCache(sources map[string][]byte) error {
	if s.cachePath == "" {
		return nil
	}
	text := make(map[string]string, len(sources))
	for path, content := range sources {
		text[path] = string(content)
	}
	raw, err := json.Marshal(text)
	if err != nil {
		return err
	}
	tmp := s.cachePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.cachePath)
}

// Get returns the current registry, which may be nil.
func (s *Set) Get() *Registry {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reg
}

// Replace installs a newly compiled registry.
func (s *Set) Replace(r *Registry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reg = r
}

// Methods lists the rpcs currently loaded, as "/pkg.Service/Method".
func (s *Set) Methods() []string {
	return s.Get().Methods()
}

// Summary describes the current registry.
func (s *Set) Summary() Summary {
	return s.Get().Summary()
}

// Load reads a FileDescriptorSet produced by protoc. An empty path disables gRPC.
func Load(path string) (*Registry, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(set.File) == 0 {
		return nil, fmt.Errorf("%s contains no file descriptors", path)
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", path, err)
	}
	return &Registry{groups: []*protoregistry.Files{files}, source: "file"}, nil
}

// Compile builds a registry from in-memory .proto sources keyed by relative path.
//
// Files are compiled one top-level folder at a time, and a folder that fails is
// reported as a warning instead of failing the whole import. Real proto trees are
// not always internally consistent, and one broken service should not cost you the rest.
func Compile(sources map[string][]byte) (*Registry, error) {
	normalized := normalize(sources)
	if len(normalized) == 0 {
		return nil, errors.New("upload contains no .proto files")
	}

	groups := map[string][]string{}
	for path := range normalized {
		group := path
		if i := strings.Index(path, "/"); i >= 0 {
			group = path[:i]
		}
		groups[group] = append(groups[group], path)
	}

	resolver := &caseTolerant{inner: protocompile.WithStandardImports(memResolver(normalized))}
	var compiled []*protoregistry.Files
	var warnings []string

	for _, group := range sortedKeys(groups) {
		compiler := protocompile.Compiler{Resolver: resolver}
		files, err := compiler.Compile(context.Background(), groups[group]...)
		if err != nil {
			warnings = append(warnings, group+": "+firstLine(err))
			continue
		}
		set := &descriptorpb.FileDescriptorSet{}
		seen := map[string]bool{}
		for _, fd := range files {
			addWithImports(set, seen, fd)
		}
		reg, err := protodesc.NewFiles(set)
		if err != nil {
			warnings = append(warnings, group+": "+firstLine(err))
			continue
		}
		compiled = append(compiled, reg)
	}
	if len(compiled) == 0 {
		return nil, fmt.Errorf("nothing compiled: %s", strings.Join(warnings, "; "))
	}
	return &Registry{groups: compiled, warnings: append(warnings, resolver.notes()...), source: "upload"}, nil
}

// caseTolerant explains an import whose filename case does not match the real file.
//
// macOS hands out google/protobuf/empty.proto for an import of Empty.proto, so this
// kind of typo compiles locally with protoc and then breaks on Linux and CI.
// protocompile refuses to serve a file under a name it was not asked for, so all
// this can do is name the mistake instead of leaving "could not resolve path".
type caseTolerant struct {
	inner     protocompile.Resolver
	mu        sync.Mutex
	seen      map[string]bool
	collected []string
}

func (c *caseTolerant) notes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.collected...)
}

func (c *caseTolerant) FindFileByPath(path string) (protocompile.SearchResult, error) {
	res, err := c.inner.FindFileByPath(path)
	if err == nil {
		return res, nil
	}

	i := strings.LastIndexByte(path, '/')
	dir, base := path[:i+1], path[i+1:]
	lower := strings.ToLower(base)
	if lower == base {
		return res, err
	}
	if _, foldErr := c.inner.FindFileByPath(dir + lower); foldErr != nil {
		return res, err
	}

	c.mu.Lock()
	if c.seen == nil {
		c.seen = map[string]bool{}
	}
	if !c.seen[path] {
		c.seen[path] = true
		c.collected = append(c.collected, fmt.Sprintf(
			"%q is imported as %q but the file is %q - imports are case-sensitive on Linux and CI",
			dir+lower, base, lower))
	}
	c.mu.Unlock()

	return res, err
}

// Lookup resolves a gRPC call path such as "/helloworld.Greeter/SayHello".
func (r *Registry) Lookup(path string) (protoreflect.MethodDescriptor, error) {
	if r == nil {
		return nil, ErrNoDescriptors
	}
	if v, ok := r.cache.Load(path); ok {
		return v.(protoreflect.MethodDescriptor), nil
	}

	service, method, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if !ok || service == "" || method == "" {
		return nil, fmt.Errorf("%q is not a /package.Service/Method path", path)
	}
	for _, files := range r.groups {
		desc, err := files.FindDescriptorByName(protoreflect.FullName(service))
		if err != nil {
			continue
		}
		svc, ok := desc.(protoreflect.ServiceDescriptor)
		if !ok {
			continue
		}
		md := svc.Methods().ByName(protoreflect.Name(method))
		if md == nil {
			return nil, fmt.Errorf("service %s has no method %s", service, method)
		}
		r.cache.Store(path, md)
		return md, nil
	}
	return nil, fmt.Errorf("unknown service %s", service)
}

// Methods lists every rpc in the descriptor set as "/package.Service/Method".
func (r *Registry) Methods() []string {
	out := []string{}
	if r == nil {
		return out
	}
	seen := map[string]bool{}
	for _, files := range r.groups {
		files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
			services := fd.Services()
			for i := 0; i < services.Len(); i++ {
				sd := services.Get(i)
				methods := sd.Methods()
				for j := 0; j < methods.Len(); j++ {
					name := "/" + string(sd.FullName()) + "/" + string(methods.Get(j).Name())
					if !seen[name] {
						seen[name] = true
						out = append(out, name)
					}
				}
			}
			return true
		})
	}
	sort.Strings(out)
	return out
}

// Summary describes the registry for the API and the UI.
func (r *Registry) Summary() Summary {
	if r == nil {
		return Summary{Services: []string{}, Methods: []string{}, Warnings: []string{}}
	}
	methods := r.Methods()
	seen := map[string]bool{}
	services := []string{}
	for _, m := range methods {
		parts := strings.Split(strings.TrimPrefix(m, "/"), "/")
		if len(parts) == 2 && !seen[parts[0]] {
			seen[parts[0]] = true
			services = append(services, parts[0])
		}
	}
	warnings := r.warnings
	if warnings == nil {
		warnings = []string{}
	}
	return Summary{Services: services, Methods: methods, Warnings: warnings, Source: r.source}
}

// DecodeMessage converts a request message to ProtoJSON, so the ordinary JSON
// matcher and the journal can treat a gRPC call like any other request body.
func (r *Registry) DecodeMessage(md protoreflect.MethodDescriptor, raw []byte) ([]byte, error) {
	msg := dynamicpb.NewMessage(md.Input())
	if err := proto.Unmarshal(raw, msg); err != nil {
		return nil, fmt.Errorf("decode %s: %w", md.Input().FullName(), err)
	}
	return protojson.MarshalOptions{}.Marshal(msg)
}

// EncodeMessage builds the response message for md from a JSON value.
func (r *Registry) EncodeMessage(md protoreflect.MethodDescriptor, v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	msg := dynamicpb.NewMessage(md.Output())
	if err := protojson.Unmarshal(raw, msg); err != nil {
		return nil, fmt.Errorf("response body does not fit %s: %w", md.Output().FullName(), err)
	}
	return proto.Marshal(msg)
}

// memResolver serves .proto sources from memory so an import works whichever
// folder the user happened to pick in the browser.
type memResolver map[string][]byte

func (m memResolver) FindFileByPath(path string) (protocompile.SearchResult, error) {
	if b, ok := m[path]; ok {
		return protocompile.SearchResult{Source: bytes.NewReader(b)}, nil
	}
	// Fall back to a unique suffix match, so uploading a parent directory works.
	suffix := "/" + path
	match := ""
	for name := range m {
		if strings.HasSuffix(name, suffix) && (match == "" || name < match) {
			match = name
		}
	}
	if match != "" {
		return protocompile.SearchResult{Source: bytes.NewReader(m[match])}, nil
	}
	return protocompile.SearchResult{}, os.ErrNotExist
}

// normalize strips the shared top-level folder so imports resolve the way the
// .proto files expect, and drops anything that is not a .proto file.
func normalize(sources map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(sources))
	prefix := commonDirPrefix(sources)
	for path, content := range sources {
		path = strings.TrimPrefix(filepath.ToSlash(path), prefix)
		path = strings.TrimPrefix(path, "/")
		if !strings.HasSuffix(path, ".proto") {
			continue
		}
		out[path] = content
	}
	return out
}

func commonDirPrefix(sources map[string][]byte) string {
	prefix := ""
	for path := range sources {
		slash := filepath.ToSlash(path)
		i := strings.Index(slash, "/")
		if i < 0 {
			return ""
		}
		segment := slash[:i+1]
		if prefix == "" {
			prefix = segment
			continue
		}
		if segment != prefix {
			return ""
		}
	}
	return prefix
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// addWithImports records fd and everything it imports, dependencies first, once each.
func addWithImports(set *descriptorpb.FileDescriptorSet, seen map[string]bool, fd protoreflect.FileDescriptor) {
	if seen[fd.Path()] {
		return
	}
	seen[fd.Path()] = true
	imports := fd.Imports()
	for i := 0; i < imports.Len(); i++ {
		addWithImports(set, seen, imports.Get(i).FileDescriptor)
	}
	set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
}

func firstLine(err error) string {
	msg := err.Error()
	if i := strings.IndexByte(msg, 0x0a); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return msg
}
