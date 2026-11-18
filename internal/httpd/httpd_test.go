package httpd

import (
	"bytes"
	"testing"
)

// parseHead must accept the request lines real clients send: HTTP/1.1
// (keep-alive by default) and HTTP/1.0 (close by default, keep-alive
// when asked). A past bug checked the version's dot at index 5 instead
// of 6 ("HTTP/1.1"[5] is '1', not '.'), which rejected every real
// request; these cases pin the indices.
func TestParseHeadVersions(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		method     string
		target     string
		version    string
		closeAfter bool
	}{
		{"http11", "GET /healthz HTTP/1.1\r\nhost: x\r\n\r\n", "GET", "/healthz", "HTTP/1.1", false},
		{"http10", "GET /healthz HTTP/1.0\r\nhost: x\r\n\r\n", "GET", "/healthz", "HTTP/1.0", true},
		{"http10keepalive", "GET /healthz HTTP/1.0\r\nhost: x\r\nconnection: keep-alive\r\n\r\n", "GET", "/healthz", "HTTP/1.0", false},
		{"http11close", "POST /mcp HTTP/1.1\r\nhost: x\r\nconnection: close\r\ncontent-length: 0\r\n\r\n", "POST", "/mcp", "HTTP/1.1", true},
		{"http20", "GET /healthz HTTP/2.0\r\nhost: x\r\n\r\n", "GET", "/healthz", "HTTP/2.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head, err := parseHead([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parseHead(%q) error: %v", tc.raw, err)
			}
			if head == nil {
				t.Fatalf("parseHead(%q) returned nil, want a head", tc.raw)
			}
			if head.method != tc.method || head.target != tc.target || head.version != tc.version {
				t.Fatalf("parseHead(%q) = %s %s %s, want %s %s %s",
					tc.raw, head.method, head.target, head.version, tc.method, tc.target, tc.version)
			}
			if head.closeAfter != tc.closeAfter {
				t.Fatalf("parseHead(%q) closeAfter = %v, want %v", tc.raw, head.closeAfter, tc.closeAfter)
			}
			if head.headers["host"] != "x" {
				t.Fatalf("parseHead(%q) host header = %q, want %q", tc.raw, head.headers["host"], "x")
			}
		})
	}
}

func TestParseHeadBadVersions(t *testing.T) {
	for _, raw := range []string{
		"GET /healthz HTTX/1.1\r\nhost: x\r\n\r\n",  // not HTTP/
		"GET /healthz HTTP/1.x\r\nhost: x\r\n\r\n",  // minor not a digit
		"GET /healthz HTTP/x.1\r\nhost: x\r\n\r\n",  // major not a digit
		"GET /healthz HTTP/11\r\nhost: x\r\n\r\n",   // no dot
		"GET /healthz HTTP/1.1x\r\nhost: x\r\n\r\n", // too long
		"GET /healthz\r\nhost: x\r\n\r\n",           // no version at all
	} {
		if head, err := parseHead([]byte(raw)); err == nil {
			t.Fatalf("parseHead(%q) = %+v, want an error", raw, head)
		}
	}
}

func TestParseHeadNeedsMore(t *testing.T) {
	if head, err := parseHead([]byte("GET /healthz HTTP/1.1\r\nhost: x\r\n")); err != nil || head != nil {
		t.Fatalf("partial head = %+v, %v; want nil, nil", head, err)
	}
}

// JSONRaw and Bytes keep their old 2- and 3-argument call shapes and
// accept an optional headers map, merged into Result.Headers.
func TestJSONRawAndBytesHeaders(t *testing.T) {
	body := []byte(`{"ok":true}`)

	r := JSONRaw(200, body)
	if r.Status != 200 || r.ContentType != "application/json" || !bytes.Equal(r.Body, body) || r.Headers != nil {
		t.Fatalf("JSONRaw(200, body) = %+v", r)
	}

	r = JSONRaw(200, body, map[string]string{"link": `<https://example.invalid/p?page=2>; rel="next"`})
	if got := r.Headers["link"]; got == "" {
		t.Fatalf("JSONRaw with headers lost them: %+v", r.Headers)
	}

	r = JSONRaw(200, body, nil, map[string]string{"a": "1"}, map[string]string{"a": "2", "b": "3"})
	if r.Headers["a"] != "2" || r.Headers["b"] != "3" {
		t.Fatalf("JSONRaw header merge = %+v, want a=2 b=3", r.Headers)
	}

	r = Bytes(200, body, "application/x-git-upload-pack-advertisement")
	if r.Status != 200 || r.ContentType != "application/x-git-upload-pack-advertisement" || r.Headers != nil {
		t.Fatalf("Bytes(200, body, ct) = %+v", r)
	}

	r = Bytes(204, []byte{}, "application/json", map[string]string{"x-github-request-id": "req-9"})
	if r.Headers["x-github-request-id"] != "req-9" {
		t.Fatalf("Bytes with headers lost them: %+v", r.Headers)
	}
}
