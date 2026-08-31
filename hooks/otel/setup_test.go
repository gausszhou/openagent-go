package otel

import (
	"net/url"
	"strings"
	"testing"
)

// TestDetectEndpointURL pins the routing decision that is the core of the
// telemetry endpoint fix. The OTel SDK splits endpoint input across two
// options: WithEndpoint (bare host:port, scheme prepended by the SDK) and
// WithEndpointURL (full URL, parsed by the SDK). A scheme-bearing input MUST
// route to WithEndpointURL — otherwise the SDK stores the whole URL as the
// Host and prepends its own scheme, producing "http://http://localhost:4318".
func TestDetectEndpointURL(t *testing.T) {
	cases := []struct {
		name  string
		input string
		val   string
		isURL bool
	}{
		{"bare host:port", "localhost:4318", "localhost:4318", false},
		{"bare host no port", "localhost", "localhost", false},
		{"bare host with path is NOT a URL", "localhost:4318/v1/traces", "localhost:4318/v1/traces", false},
		{"http URL", "http://localhost:4318", "http://localhost:4318", true},
		{"https URL", "https://collector.example:4318/otlp", "https://collector.example:4318/otlp", true},
		{"uppercase scheme", "HTTP://Localhost:4318", "HTTP://Localhost:4318", true},
		{"leading spaces trimmed", "  http://localhost:4318  ", "http://localhost:4318", true},
		{"empty", "", "", false},
		{"scheme-ish but not http(s)", "ftp://localhost:4318", "ftp://localhost:4318", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, isURL := detectEndpointURL(c.input)
			if got != c.val || isURL != c.isURL {
				t.Errorf("detectEndpointURL(%q) = (%q, %v), want (%q, %v)",
					c.input, got, isURL, c.val, c.isURL)
			}
		})
	}
}

// TestEndpointURLNoDoubleScheme is the regression test for the original bug:
// an endpoint configured as "http://xxx" produced a double scheme prefix
// ("http://http://localhost:4318/v1/traces") because the code passed the full
// URL to WithEndpoint, which stores it verbatim as the Host and lets the SDK
// prepend its own scheme.
//
// It reconstructs the OTel HTTP exporter's URL assembly faithfully:
//
//	u := url.URL{Scheme: getScheme(), Host: cfg.Endpoint, Path: cfg.URLPath}
//
// where, post-fix, a URL endpoint has been routed through WithEndpointURL —
// which parses the input and stores only u.Host in cfg.Endpoint and u.Path in
// cfg.URLPath (falling back to the default "/v1/traces" when empty, exactly
// as the SDK's cleanPath does). The assembled URL must be well-formed and
// contain exactly one scheme separator.
func TestEndpointURLNoDoubleScheme(t *testing.T) {
	const defaultPath = "/v1/traces"

	// assemble mirrors the SDK's resolved URL given the routing decision
	// detectEndpointURL makes. For a URL endpoint it applies the same
	// host/path extraction WithEndpointURL performs internally; for a bare
	// endpoint it uses the value as-is (scheme from Insecure, path default).
	assemble := func(input string) string {
		endpoint, isURL := detectEndpointURL(input)
		scheme := "http" // Insecure=true default
		host := endpoint
		path := defaultPath
		if isURL {
			u, err := url.Parse(endpoint)
			if err != nil {
				return "<parse error>"
			}
			host = u.Host
			if strings.TrimSpace(u.Path) == "" || u.Path == "." {
				path = defaultPath // SDK cleanPath behavior
			} else {
				path = u.Path
				if !strings.HasPrefix(path, "/") {
					path = "/" + path
				}
			}
			if u.Scheme == "https" {
				scheme = "https"
			}
		}
		u := url.URL{Scheme: scheme, Host: host, Path: path}
		return u.String()
	}

	cases := []struct {
		input string
		want  string
	}{
		{"http://localhost:4318", "http://localhost:4318/v1/traces"},
		{"https://collector.example:4318/otlp", "https://collector.example:4318/otlp"},
		{"localhost:4318", "http://localhost:4318/v1/traces"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			got := assemble(c.input)
			if got != c.want {
				t.Fatalf("assembled endpoint for %q = %q, want %q", c.input, got, c.want)
			}
			// The regression invariant, in the user's own terms: never a
			// duplicated "://" — i.e. never two scheme prefixes stacked.
			if strings.Count(got, "://") != 1 {
				t.Fatalf("assembled endpoint %q has a duplicated scheme prefix (got %d scheme separators, want 1)",
					got, strings.Count(got, "://"))
			}
		})
	}
}
