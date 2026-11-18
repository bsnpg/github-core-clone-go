// Package httpd is the forge's raw TCP HTTP/1.1 server: a hand-rolled
// reader on net, not net/http, plus a tiny router and the response helpers
// the feature packages build on.
//
// The transport is a port of the Node entry's app.ts reader. The suite's
// workloads are thousands of small sequential requests on keep-alive
// connections, so each request costs one parse and one write: the request
// line plus headers are parsed once out of the connection buffer, the
// handler runs synchronously once a whole request is buffered (pipelined
// requests on one socket are answered in order), and the response goes out
// as a single Write. No per-request allocation beyond the request itself,
// TCP_NODELAY on every connection for a fast wakeup.
package httpd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"forge/internal/util"
)

// MaxBodyBytes caps a request body, MaxHeaderBytes a request head.
const (
	MaxBodyBytes   = 64 * 1024 * 1024
	MaxHeaderBytes = 64 * 1024
)

// reasonPhrases are the status lines this server ever writes.
var reasonPhrases = map[int]string{
	200: "OK",
	201: "Created",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

// Ctx is one request, as the router hands it to a handler. Headers keys
// are lower-cased. Query is the raw query string, without the '?'. Path
// is the raw request path, without the query string.
type Ctx struct {
	Method    string
	Path      string
	Query     string
	Headers   map[string]string
	Body      []byte
	RequestID string
	Authed    bool
	Token     string
}

// Result is one handler answer. Headers are extra response headers
// (lower-case names); the transport always adds content-type,
// content-length and x-github-request-id itself.
type Result struct {
	Status      int
	ContentType string
	Body        []byte
	Headers     map[string]string
}

// Handler answers one request.
type Handler func(c *Ctx) Result

// JSON marshals v as JSON with a trailing newline, the way the Node entry
// answers every JSON body. HTML escaping is off, so a body round-trips
// the way encoding it in Node would.
func JSON(status int, v any) Result {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return Result{Status: 500, ContentType: "application/json", Body: []byte("{\"message\":\"Internal Server Error\"}\n")}
	}
	return Result{Status: status, ContentType: "application/json", Body: buf.Bytes()}
}

// JSONRaw sends body as-is with a JSON content type, for a pre-stringified
// body (a cached view) that must not gain a second newline or a second
// marshal. An optional headers map is merged into Result.Headers, so a
// caller that already built its headers (Link, X-OAuth-Scopes) can pass
// them along with the raw body.
func JSONRaw(status int, body []byte, headers ...map[string]string) Result {
	return Result{Status: status, ContentType: "application/json", Body: body, Headers: mergeHeaders(headers)}
}

// Bytes sends body with the caller's content type (git smart HTTP). An
// optional headers map is merged into Result.Headers.
func Bytes(status int, body []byte, contentType string, headers ...map[string]string) Result {
	return Result{Status: status, ContentType: contentType, Body: body, Headers: mergeHeaders(headers)}
}

// mergeHeaders merges zero or more header maps into one, later maps
// winning on a clash. The result is a fresh map (or nil when nothing was
// passed), so a handler's shared map is never aliased into a response
// another goroutine might read.
func mergeHeaders(maps []map[string]string) map[string]string {
	var out map[string]string
	for _, m := range maps {
		if len(m) == 0 {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(m))
		}
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// errResult is a JSON error body, the shape every error in the forge uses.
func errResult(status int, message string) Result {
	return JSON(status, map[string]any{"message": message})
}

// route is one registered handler. pattern holds the path for an exact
// route, or the prefix (without the trailing "*") for a prefix route.
type route struct {
	method  string
	pattern string
	prefix  bool
	h       Handler
}

// Router matches requests to handlers. Handle registers a route: pattern
// is an exact path, or a prefix with a "*" suffix (for example
// "/api/v3/*"). A method of "*" matches every method. Matching prefers an
// exact path over a prefix, and among prefixes the longest one; the first
// registration wins a tie. A request whose path matches an exact route
// under another method is answered 405, never 404.
type Router struct {
	routes []route
	// Middleware, when set, wraps every matched handler. The entrypoint
	// sets it to attach the request id and the auth verdict before any
	// feature handler runs.
	Middleware func(Handler) Handler
	// NotFound, when set, answers requests no route matches. The default
	// is a JSON 404.
	NotFound Handler
}

// Handle registers h for method and pattern.
func (r *Router) Handle(method, pattern string, h Handler) {
	rt := route{method: method, h: h}
	if strings.HasSuffix(pattern, "*") {
		rt.prefix = true
		rt.pattern = pattern[:len(pattern)-1]
	} else {
		rt.pattern = pattern
	}
	r.routes = append(r.routes, rt)
}

// invoke runs h through the middleware, if any.
func (r *Router) invoke(h Handler, c *Ctx) Result {
	if r.Middleware != nil {
		h = r.Middleware(h)
	}
	return h(c)
}

// dispatch routes one request. It never panics on handler code: a handler
// that throws is answered 500, the way the Node dispatcher does it.
func (r *Router) dispatch(c *Ctx) (res Result) {
	if c.RequestID == "" {
		c.RequestID = util.NextRequestID()
	}
	path := c.Path
	// Absolute-form targets (a client talking through a proxy) carry the
	// origin in front; strip it defensively, as the Node entry does.
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		if rest := path[strings.Index(path, "://")+3:]; strings.IndexByte(rest, '/') >= 0 {
			path = rest[strings.IndexByte(rest, '/'):]
		} else {
			path = "/"
		}
	}
	c.Path = path

	defer func() {
		if rec := recover(); rec != nil {
			res = errResult(500, "Internal Server Error")
		}
	}()

	// Exact path first, whatever the method.
	pathMatched := false
	for _, rt := range r.routes {
		if rt.prefix || rt.pattern != path {
			continue
		}
		pathMatched = true
		if rt.method == "*" || rt.method == c.Method {
			return r.invoke(rt.h, c)
		}
	}
	if pathMatched {
		return r.invoke(func(c *Ctx) Result {
			return errResult(405, "Method Not Allowed")
		}, c)
	}

	// Longest matching prefix.
	best := -1
	bestLen := -1
	for i, rt := range r.routes {
		if !rt.prefix {
			continue
		}
		if rt.method != "*" && rt.method != c.Method {
			continue
		}
		if strings.HasPrefix(path, rt.pattern) || path == strings.TrimSuffix(rt.pattern, "/") {
			if len(rt.pattern) > bestLen {
				best, bestLen = i, len(rt.pattern)
			}
		}
	}
	if best >= 0 {
		return r.invoke(r.routes[best].h, c)
	}

	if r.NotFound != nil {
		return r.invoke(r.NotFound, c)
	}
	return r.invoke(func(c *Ctx) Result {
		return errResult(404, "Not Found")
	}, c)
}

// Serve accepts connections on ln until it closes. Each connection gets
// its own goroutine; the requests on one connection run one at a time, in
// order.
func (r *Router) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return err
		}
		go r.serveConn(conn)
	}
}

// requestHead is a request's line and headers, parsed before its body.
type requestHead struct {
	method     string
	target     string
	version    string
	headers    map[string]string
	bodyStart  int
	closeAfter bool
	expect100  bool
}

// parseHead parses the head out of buf. It returns nil, nil when more
// bytes are needed, and a non-nil error on a malformed head.
func parseHead(buf []byte) (*requestHead, error) {
	end := bytes.Index(buf, []byte("\r\n\r\n"))
	if end == -1 {
		if len(buf) > MaxHeaderBytes {
			return nil, errors.New("request headers too large")
		}
		return nil, nil
	}
	head := string(buf[:end])
	lines := strings.Split(head, "\r\n")
	parts := strings.SplitN(lines[0], " ", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" ||
		len(parts[2]) != 8 || !strings.HasPrefix(parts[2], "HTTP/") ||
		!isDigit(parts[2][5]) || parts[2][6] != '.' || !isDigit(parts[2][7]) {
		return nil, errors.New("malformed request line")
	}
	headers := make(map[string]string, len(lines)-1)
	for _, line := range lines[1:] {
		colon := strings.IndexByte(line, ':')
		if colon == -1 {
			return nil, errors.New("malformed header line")
		}
		name := strings.ToLower(strings.TrimSpace(line[:colon]))
		if name == "" {
			continue
		}
		headers[name] = strings.TrimSpace(line[colon+1:])
	}
	connection := headers["connection"]
	version := parts[2]
	closeAfter := strings.Contains(strings.ToLower(connection), "close") ||
		(version == "HTTP/1.0" && !strings.Contains(strings.ToLower(connection), "keep-alive"))
	return &requestHead{
		method:     parts[0],
		target:     parts[1],
		version:    version,
		headers:    headers,
		bodyStart:  end + 4,
		closeAfter: closeAfter,
		expect100:  strings.EqualFold(strings.TrimSpace(headers["expect"]), "100-continue"),
	}, nil
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// parseBody extracts the body for a parsed head out of buf. It returns
// nil, 0, nil when more bytes are needed, and a non-nil error on a
// malformed body. consumed counts the whole request.
func parseBody(head *requestHead, buf []byte) ([]byte, int, error) {
	if strings.Contains(strings.ToLower(head.headers["transfer-encoding"]), "chunked") {
		return parseChunked(buf, head.bodyStart)
	}
	lengthText, ok := head.headers["content-length"]
	if !ok {
		return []byte{}, head.bodyStart, nil
	}
	length, err := strconv.Atoi(strings.TrimSpace(lengthText))
	if err != nil || length < 0 {
		return nil, 0, errors.New("malformed content-length")
	}
	if length > MaxBodyBytes {
		return nil, 0, errors.New("request body too large")
	}
	if len(buf) < head.bodyStart+length {
		return nil, 0, nil
	}
	return buf[head.bodyStart : head.bodyStart+length], head.bodyStart + length, nil
}

// parseChunked decodes chunked framing into one body. Every chunk size
// line and trailer is ASCII.
func parseChunked(buf []byte, at int) ([]byte, int, error) {
	var chunks [][]byte
	total := 0
	for {
		lineEnd := bytes.Index(buf[at:], []byte("\r\n"))
		if lineEnd == -1 {
			return nil, 0, nil
		}
		lineEnd += at
		sizeText := strings.TrimSpace(strings.SplitN(string(buf[at:lineEnd]), ";", 2)[0])
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil || size < 0 {
			return nil, 0, errors.New("malformed chunk size")
		}
		dataStart := lineEnd + 2
		if size == 0 {
			// Trailer headers, if any, end at the next blank line.
			trailerEnd := bytes.Index(buf[dataStart:], []byte("\r\n\r\n"))
			end := dataStart + 2
			if trailerEnd != -1 {
				end = dataStart + trailerEnd + 4
			}
			if len(buf) < end {
				return nil, 0, nil
			}
			out := make([]byte, 0, total)
			for _, chunk := range chunks {
				out = append(out, chunk...)
			}
			return out, end, nil
		}
		if total+int(size) > MaxBodyBytes {
			return nil, 0, errors.New("request body too large")
		}
		if len(buf) < dataStart+int(size)+2 {
			return nil, 0, nil
		}
		if buf[dataStart+int(size)] != '\r' || buf[dataStart+int(size)+1] != '\n' {
			return nil, 0, errors.New("malformed chunk framing")
		}
		chunks = append(chunks, buf[dataStart:dataStart+int(size)])
		total += int(size)
		at = dataStart + int(size) + 2
	}
}

// buildResponse serialises one answer: status line, headers,
// Content-Length, body — in a single buffer, so the caller writes it with
// one Write. The x-github-request-id header is added here, from the
// request id, without mutating the handler's Result (a handler may return
// a shared Result).
func buildResponse(res Result, requestID string) []byte {
	body := res.Body
	if res.Status == 204 || res.Status == 304 {
		body = nil
	}
	reason := reasonPhrases[res.Status]
	head := make([]byte, 0, 128)
	head = append(head, "HTTP/1.1 "...)
	head = strconv.AppendInt(head, int64(res.Status), 10)
	head = append(head, ' ')
	head = append(head, reason...)
	head = append(head, "\r\ncontent-type: "...)
	head = append(head, res.ContentType...)
	head = append(head, "\r\ncontent-length: "...)
	head = strconv.AppendInt(head, int64(len(body)), 10)
	head = append(head, "\r\n"...)
	if _, ok := res.Headers["x-github-request-id"]; !ok && requestID != "" {
		head = append(head, "x-github-request-id: "...)
		head = append(head, requestID...)
		head = append(head, "\r\n"...)
	}
	if len(res.Headers) > 0 {
		names := make([]string, 0, len(res.Headers))
		for name := range res.Headers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			head = append(head, name...)
			head = append(head, ": "...)
			head = append(head, res.Headers[name]...)
			head = append(head, "\r\n"...)
		}
	}
	head = append(head, "\r\n"...)
	if len(body) == 0 {
		return head
	}
	return append(head, body...)
}

// fail writes a 400 for a malformed request and reports that the
// connection must be destroyed.
func fail(conn net.Conn, message string) {
	_, _ = conn.Write(buildResponse(errResult(400, message), ""))
}

// serveConn reads requests off one connection and answers them in order.
// It returns when the peer closes, asks to close, or sends a malformed
// request.
func (r *Router) serveConn(conn net.Conn) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	var pending []byte
	chunk := make([]byte, 32*1024)
	// sent100 tracks whether the interim 100 for the request currently
	// being buffered has gone out already, so a head re-parsed after a
	// short read is not answered twice.
	sent100 := false
	for {
		// Drain every complete request already buffered.
		for {
			head, err := parseHead(pending)
			if err != nil {
				fail(conn, "malformed request")
				return
			}
			if head == nil {
				break
			}
			// Answer an Expect: 100-continue as soon as the head is
			// parsed, before waiting on the body.
			if head.expect100 && !sent100 {
				_, _ = conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
				sent100 = true
			}
			body, consumed, err := parseBody(head, pending)
			if err != nil {
				fail(conn, "malformed request")
				return
			}
			if body == nil {
				break
			}
			pending = pending[consumed:]
			sent100 = false
			question := strings.IndexByte(head.target, '?')
			rawPath, query := head.target, ""
			if question != -1 {
				rawPath, query = head.target[:question], head.target[question+1:]
			}
			ctx := &Ctx{
				Method:  head.method,
				Path:    rawPath,
				Query:   query,
				Headers: head.headers,
				Body:    body,
			}
			res := r.dispatch(ctx)
			if _, err := conn.Write(buildResponse(res, ctx.RequestID)); err != nil {
				return
			}
			if head.closeAfter {
				return
			}
		}
		if len(pending) == 0 {
			pending = pending[:0]
		}
		n, err := conn.Read(chunk)
		if n > 0 {
			pending = append(pending, chunk[:n]...)
		}
		if err != nil {
			if err != io.EOF {
				// A read error mid-request ends the connection; there
				// is nothing left to answer.
			}
			return
		}
	}
}
