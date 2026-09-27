package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/syahrizalfauzi/jomock/internal/store"
	"github.com/syahrizalfauzi/jomock/internal/stub"
)

func newStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stubs.json")
	st := store.New(path, 50)
	if err := st.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	return st, path
}

func newServers(t *testing.T, st *store.Store) (admin, mock string) {
	t.Helper()
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	web := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>ui</html>")}}
	adminSrv := httptest.NewServer(NewAdmin(st, nil, web, lg))
	mockSrv := httptest.NewServer(NewMock(st, nil, lg))
	t.Cleanup(adminSrv.Close)
	t.Cleanup(mockSrv.Close)
	return adminSrv.URL, mockSrv.URL
}

func do(t *testing.T, method, url, body string) (int, http.Header, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(b)
}

func TestEndToEnd(t *testing.T) {
	st, path := newStore(t)
	admin, mock := newServers(t, st)

	if code, _, body := do(t, "GET", admin+"/", ""); code != http.StatusOK || !strings.Contains(body, "ui") {
		t.Fatalf("UI: code=%d body=%q", code, body)
	}

	const stubJSON = `{
	  "request": {"method": "GET", "path": "/hello", "query": {"x": "1"}},
	  "response": {"status": 201, "headers": {"X-Mock": "yes"}, "body": "hi there"}
	}`

	code, _, body := do(t, "POST", admin+"/__admin/mappings", stubJSON)
	if code != http.StatusCreated {
		t.Fatalf("create stub: code=%d body=%s", code, body)
	}
	var created stub.Stub
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode created stub: %v", err)
	}
	if created.ID == "" {
		t.Fatal("server did not assign an id")
	}

	code, header, body := do(t, "GET", mock+"/hello?x=1", "")
	if code != 201 || body != "hi there" || header.Get("X-Mock") != "yes" {
		t.Fatalf("matched request: code=%d header=%q body=%q", code, header.Get("X-Mock"), body)
	}

	if code, _, body := do(t, "GET", mock+"/hello?x=2", ""); code != http.StatusNotFound {
		t.Fatalf("query mismatch should 404, got %d %s", code, body)
	}

	code, _, body = do(t, "GET", admin+"/__admin/requests", "")
	if code != http.StatusOK {
		t.Fatalf("journal: code=%d", code)
	}
	var entries []store.Entry
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatalf("decode journal: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("journal length = %d, want 2", len(entries))
	}
	if entries[0].Status != http.StatusNotFound || entries[1].StubID != created.ID {
		t.Fatalf("journal newest-first wrong: %+v", entries)
	}

	verify := func(extra string, wantPass bool) {
		t.Helper()
		_, _, body := do(t, "POST", admin+"/__admin/verify", `{"stubId":"`+created.ID+`"`+extra+`}`)
		var got struct {
			Matched int  `json:"matched"`
			Passed  bool `json:"passed"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode verify: %v", err)
		}
		if got.Passed != wantPass {
			t.Fatalf("verify(%q) passed=%v matched=%d, want passed=%v", extra, got.Passed, got.Matched, wantPass)
		}
	}
	verify("", true)
	verify(`,"expectedCount":1`, true)
	verify(`,"expectedCount":5`, false)

	const delayJSON = `{"request":{"path":"/slow"},"response":{"status":200,"delayMs":80}}`
	if code, _, body := do(t, "POST", admin+"/__admin/mappings", delayJSON); code != http.StatusCreated {
		t.Fatalf("create delay stub: %d %s", code, body)
	}
	start := time.Now()
	if code, _, _ := do(t, "GET", mock+"/slow", ""); code != 200 {
		t.Fatalf("delay stub code=%d", code)
	}
	if elapsed := time.Since(start); elapsed < 70*time.Millisecond {
		t.Fatalf("delayMs not applied: %v", elapsed)
	}

	const jsonStub = `{"request":{"path":"/json"},"response":{"status":200,"jsonBody":{"ok":true,"items":[1,2]}}}`
	if code, _, body := do(t, "POST", admin+"/__admin/mappings", jsonStub); code != http.StatusCreated {
		t.Fatalf("create json stub: %d %s", code, body)
	}
	code, header, body = do(t, "GET", mock+"/json", "")
	if code != http.StatusOK || !strings.HasPrefix(header.Get("Content-Type"), "application/json") {
		t.Fatalf("jsonBody response: code=%d type=%q", code, header.Get("Content-Type"))
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("jsonBody is not valid JSON: %v (%q)", err, body)
	}
	if decoded["ok"] != true {
		t.Fatalf("jsonBody served %q, decoded %+v", body, decoded)
	}

	// a stub must declare a protocol we understand
	if code, _, _ := do(t, "POST", admin+"/__admin/mappings", `{"type":"smoke-signal","request":{"path":"/x"}}`); code != http.StatusBadRequest {
		t.Fatalf("unknown stub type should 400, got %d", code)
	}

	// rejections
	if code, _, _ := do(t, "POST", admin+"/__admin/mappings", `{"request":{"pathPattern":"[unclosed"}}`); code != http.StatusBadRequest {
		t.Fatalf("bad regex should 400, got %d", code)
	}
	if code, _, _ := do(t, "GET", admin+"/__admin/mappings/nope", ""); code != http.StatusNotFound {
		t.Fatalf("unknown id should 404, got %d", code)
	}

	// persistence
	reloaded := store.New(path, 10)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := len(reloaded.List()); got != 3 {
		t.Fatalf("reloaded %d stubs, want 3", got)
	}

	// bulk replace, behind the UI "Open" button
	const replaced = `[{"request":{"path":"/only"},"response":{"status":200,"body":"one"}}]`
	if code, _, body := do(t, "PUT", admin+"/__admin/mappings", replaced); code != http.StatusOK {
		t.Fatalf("replace mappings: %d %s", code, body)
	}
	if code, _, _ := do(t, "GET", mock+"/json", ""); code != http.StatusNotFound {
		t.Fatalf("stub should be gone after replace, got %d", code)
	}
	if code, _, body := do(t, "GET", mock+"/only", ""); code != http.StatusOK || body != "one" {
		t.Fatalf("replaced stub: %d %q", code, body)
	}

	// a rejected replace must not touch the current list
	if code, _, _ := do(t, "PUT", admin+"/__admin/mappings", `[{"request":{"pathPattern":"[unclosed"}}]`); code != http.StatusBadRequest {
		t.Fatalf("invalid replace should 400, got %d", code)
	}
	if got := len(st.List()); got != 1 {
		t.Fatalf("failed replace mutated the list: %d stubs", got)
	}
}
