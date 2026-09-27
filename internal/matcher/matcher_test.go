package matcher

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/syahrizalfauzi/jomock/internal/stub"
)

func pattern(t *testing.T, raw string) stub.RequestPattern {
	t.Helper()
	var p stub.RequestPattern
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("bad pattern %s: %v", raw, err)
	}
	return p
}

func TestMatch(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		method  string
		url     string
		headers map[string]string
		body    string
		want    bool
	}{
		{"method+path hit", `{"method":"GET","path":"/a"}`, "GET", "/a", nil, "", true},
		{"method miss", `{"method":"GET","path":"/a"}`, "POST", "/a", nil, "", false},
		{"path miss", `{"method":"GET","path":"/a"}`, "GET", "/b", nil, "", false},
		{"method case-insensitive", `{"method":"get"}`, "GET", "/anything", nil, "", true},
		{"empty pattern matches anything", `{}`, "DELETE", "/whatever", nil, "", true},
		{"pathPattern hit", `{"pathPattern":"^/users/[0-9]+$"}`, "GET", "/users/42", nil, "", true},
		{"pathPattern miss", `{"pathPattern":"^/users/[0-9]+$"}`, "GET", "/users/bob", nil, "", false},
		{"query subset hit", `{"query":{"a":"1"}}`, "GET", "/q?a=1&b=2", nil, "", true},
		{"query subset miss", `{"query":{"a":"1"}}`, "GET", "/q?a=9", nil, "", false},
		{"header subset hit", `{"headers":{"X-Key":"v"}}`, "GET", "/h", map[string]string{"x-key": "v"}, "", true},
		{"header miss", `{"headers":{"X-Key":"v"}}`, "GET", "/h", map[string]string{"X-Key": "nope"}, "", false},
		{"body equal hit", `{"body":{"equal":"raw"}}`, "POST", "/b", nil, "raw", true},
		{"body equal miss", `{"body":{"equal":"raw"}}`, "POST", "/b", nil, "other", false},
		{"body contains hit", `{"body":{"contains":"needle"}}`, "POST", "/b", nil, "a needle here", true},
		{"body equalJson hit", `{"body":{"equalJson":{"a":[1,2]}}}`, "POST", "/b", nil, `{"a":[1,2]}`, true},
		{"body equalJson ignores key order", `{"body":{"equalJson":{"a":1,"b":2}}}`, "POST", "/b", nil, `{"b":2,"a":1}`, true},
		{"body equalJson miss", `{"body":{"equalJson":{"a":1}}}`, "POST", "/b", nil, `{"a":2}`, false},
		{"body equalJson on non-json", `{"body":{"equalJson":{"a":1}}}`, "POST", "/b", nil, "not json", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.url, strings.NewReader(tc.body))
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			if got := Match(pattern(t, tc.pattern), req, []byte(tc.body)); got != tc.want {
				t.Fatalf("Match = %v, want %v", got, tc.want)
			}
		})
	}
}
