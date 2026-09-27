// Package matcher decides whether a request matches a stub pattern.
// It is pure: no I/O, no server state — the whole point is that it is trivially testable.
package matcher

import (
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"strings"

	"github.com/syahrizalfauzi/jomock/internal/stub"
)

// Match reports whether req matches p. body is the already-read request body.
func Match(p stub.RequestPattern, req *http.Request, body []byte) bool {
	if p.Method != "" && !strings.EqualFold(p.Method, req.Method) {
		return false
	}

	switch {
	case p.PathPattern != "":
		re, err := regexp.Compile(p.PathPattern)
		if err != nil || !re.MatchString(req.URL.Path) {
			return false
		}
	case p.Path != "":
		if p.Path != req.URL.Path {
			return false
		}
	}

	query := req.URL.Query()
	for k, want := range p.Query {
		if !hasValue(query[k], want) {
			return false
		}
	}
	for k, want := range p.Headers {
		if !hasValue(req.Header.Values(k), want) {
			return false
		}
	}

	return matchBody(p.Body, body)
}

func hasValue(values []string, want string) bool {
	for _, v := range values {
		if strings.TrimSpace(v) == want {
			return true
		}
	}
	return false
}

func matchBody(m *stub.BodyMatch, body []byte) bool {
	if m == nil {
		return true
	}
	if m.Equal != "" && string(body) != m.Equal {
		return false
	}
	if m.Contains != "" && !strings.Contains(string(body), m.Contains) {
		return false
	}
	if m.EqualJSON != nil {
		var have any
		if err := json.Unmarshal(body, &have); err != nil {
			return false
		}
		if !reflect.DeepEqual(m.EqualJSON, have) {
			return false
		}
	}
	return true
}
