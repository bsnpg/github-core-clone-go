// The Go entrypoint: the env glue, ported from the Node entry's main.ts.
//
// The environment hands us its address through MAKEFUN_ENV_URL and
// expects three things, in order: a server listening on a port the kernel
// chose, one POST announcing that port, and then patient polling until the
// environment says the trial is finished. The grading only looks at the
// server; this file is just the glue that keeps the two sides connected.
//
// The choreography, step by step:
//   - No env address, no run: without MAKEFUN_ENV_URL we quit at once.
//   - The server binds port 0. The OS hands back something free; 8080 is
//     the environment's own port, so we never ask for it.
//   - We announce the chosen port once. A 200 means the environment took
//     it; a 409 means some other entry announced first, and we simply
//     carry on waiting like everyone else. A 400 or 405 means we broke the
//     contract, so we stop. Anything else (a dropped connection, a slow
//     environment, a surprising status) is worth another try a little later.
//   - We ask GET /done again and again until it answers {"done": true}.
//     While the suite is still running it answers {"done": false} right
//     away, so this is ordinary polling, not a hanging request.
//   - Once done, we shut the server down and leave with status 0.
//
// Nothing here reaches for net/http: the two env calls go over a small
// raw client on net, the same transport family as the server itself.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"forge/internal/githttp"
	"forge/internal/graphql"
	"forge/internal/httpd"
	"forge/internal/mcp"
	"forge/internal/rest"
	"forge/internal/store"
	"forge/internal/util"
)

// One HTTP round trip gets at most thirty seconds before we give up on it
// and try again.
const requestBudget = 30 * time.Second

// Retries start a quarter of a second apart, double each time, and never
// wait longer than two seconds.
const (
	backoffStart = 250 * time.Millisecond
	backoffCap   = 2 * time.Second
)

// backoff hands out the next wait: start small, double, stop growing at
// the cap. Callers keep one per retry loop.
type backoff struct {
	wait time.Duration
}

func newBackoff() *backoff { return &backoff{wait: backoffStart} }

func (b *backoff) next() {
	time.Sleep(b.wait)
	if b.wait < backoffCap {
		b.wait *= 2
		if b.wait > backoffCap {
			b.wait = backoffCap
		}
	}
}

// -------------------------------------------------------------------------------------------
// the dispatcher: request id + auth, wrapped around every handler
// -------------------------------------------------------------------------------------------

// bearerToken pulls the token out of an Authorization header, or reports
// there is none.
func bearerToken(headers map[string]string) (string, bool) {
	header, ok := headers["authorization"]
	if !ok {
		return "", false
	}
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(header, "Bearer ") {
		return "", false
	}
	return strings.TrimSpace(header[len("Bearer "):]), true
}

// storeMu guards the store across packages: the feature packages each
// lock their own work, but a REST mutation and a git read in flight at
// the same time would still race the store's maps. The dispatcher is the
// one choke point every request passes through, so it takes the lock
// here: reads share it (GETs, and POST git-upload-pack which only reads),
// everything else excludes. The 50-concurrent-clone workload is all
// reads, so it is not serialized by this.
var storeMu sync.RWMutex

// dispatcher is the router middleware: it stamps the deterministic
// per-process request id on the context and decides the auth verdict.
// The token owner's identity is fixed; when MAKEFUN_SHOP_TOKEN is set
// only that token authenticates, when it is unset any non-empty bearer
// token does.
func dispatcher(next httpd.Handler) httpd.Handler {
	expected := os.Getenv("MAKEFUN_SHOP_TOKEN")
	return func(c *httpd.Ctx) httpd.Result {
		if c.RequestID == "" {
			c.RequestID = util.NextRequestID()
		}
		c.Authed = false
		c.Token = ""
		if token, ok := bearerToken(c.Headers); ok && token != "" {
			c.Token = token
			if expected == "" || token == expected {
				c.Authed = true
			}
		}
		readOnly := c.Method == "GET" || (c.Method == "POST" && strings.HasSuffix(c.Path, "/git-upload-pack"))
		if readOnly {
			storeMu.RLock()
			defer storeMu.RUnlock()
		} else {
			storeMu.Lock()
			defer storeMu.Unlock()
		}
		return next(c)
	}
}

// -------------------------------------------------------------------------------------------
// the env client: two calls over raw net, no net/http
// -------------------------------------------------------------------------------------------

// rawRequest performs one HTTP/1.1 request against rawURL and returns the
// status and body. The connection is closed after the answer, so the body
// is read to EOF when no length is given. The whole round trip is capped
// at the request budget.
func rawRequest(method, rawURL string, body []byte) (int, []byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, requestBudget)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(requestBudget))

	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	var req bytes.Buffer
	req.WriteString(method + " " + path + " HTTP/1.1\r\n")
	req.WriteString("host: " + u.Host + "\r\n")
	if body != nil {
		req.WriteString("content-type: application/json\r\n")
		req.WriteString("content-length: " + strconv.Itoa(len(body)) + "\r\n")
	}
	req.WriteString("connection: close\r\n\r\n")
	req.Write(body)
	if _, err := conn.Write(req.Bytes()); err != nil {
		return 0, nil, err
	}
	raw, err := readAll(conn)
	if err != nil {
		return 0, nil, err
	}
	return parseResponse(raw)
}

// readAll reads until EOF or the deadline.
func readAll(conn net.Conn) ([]byte, error) {
	var out bytes.Buffer
	chunk := make([]byte, 32*1024)
	for {
		n, err := conn.Read(chunk)
		if n > 0 {
			out.Write(chunk[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out.Bytes(), nil
			}
			return nil, err
		}
	}
}

// parseResponse splits a raw HTTP response into its status and body,
// decoding a chunked body when the server framed one.
func parseResponse(raw []byte) (int, []byte, error) {
	headEnd := bytes.Index(raw, []byte("\r\n\r\n"))
	if headEnd == -1 {
		return 0, nil, fmt.Errorf("env answered without headers")
	}
	head := string(raw[:headEnd])
	lines := strings.Split(head, "\r\n")
	parts := strings.SplitN(lines[0], " ", 3)
	if len(parts) < 2 {
		return 0, nil, fmt.Errorf("env answered a malformed status line")
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, nil, fmt.Errorf("env answered a malformed status")
	}
	body := raw[headEnd+4:]
	headers := map[string]string{}
	for _, line := range lines[1:] {
		if i := strings.IndexByte(line, ':'); i != -1 {
			headers[strings.ToLower(strings.TrimSpace(line[:i]))] = strings.TrimSpace(line[i+1:])
		}
	}
	if strings.Contains(strings.ToLower(headers["transfer-encoding"]), "chunked") {
		decoded, _, err := decodeChunked(body)
		if err != nil {
			return 0, nil, err
		}
		return status, decoded, nil
	}
	if lengthText, ok := headers["content-length"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(lengthText)); err == nil && n >= 0 && n <= len(body) {
			return status, body[:n], nil
		}
	}
	return status, body, nil
}

// decodeChunked decodes chunked framing into one body.
func decodeChunked(buf []byte) ([]byte, int, error) {
	var out []byte
	at := 0
	for {
		lineEnd := bytes.Index(buf[at:], []byte("\r\n"))
		if lineEnd == -1 {
			return nil, 0, fmt.Errorf("env sent a truncated chunked body")
		}
		lineEnd += at
		sizeText := strings.TrimSpace(strings.SplitN(string(buf[at:lineEnd]), ";", 2)[0])
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil || size < 0 {
			return nil, 0, fmt.Errorf("env sent a malformed chunk size")
		}
		dataStart := lineEnd + 2
		if size == 0 {
			return out, dataStart + 2, nil
		}
		if len(buf) < dataStart+int(size)+2 {
			return nil, 0, fmt.Errorf("env sent a truncated chunked body")
		}
		out = append(out, buf[dataStart:dataStart+int(size)]...)
		at = dataStart + int(size) + 2
	}
}

// -------------------------------------------------------------------------------------------
// register + done
// -------------------------------------------------------------------------------------------

// announcePort posts the bound port until the environment takes it.
// 200/409 mean registered; 400/405 are fatal; anything else retries.
func announcePort(envOrigin string, port int) error {
	bo := newBackoff()
	payload := []byte(`{"port":` + strconv.Itoa(port) + `}`)
	for {
		status, body, err := rawRequest("POST", envOrigin+"/register", payload)
		if err != nil {
			// The environment is unreachable or still starting: wait,
			// then retry.
			bo.next()
			continue
		}
		if status == 200 || status == 409 {
			return nil
		}
		if status == 400 || status == 405 {
			return fmt.Errorf("register answered HTTP %d: %s", status, strings.TrimSpace(string(body)))
		}
		bo.next()
	}
}

// awaitCompletion polls GET /done until the environment says the trial
// is over. A "not done" answer, a failed request and a slow request are
// all just reasons to ask again after a short wait.
func awaitCompletion(envOrigin string) {
	bo := newBackoff()
	for {
		status, body, err := rawRequest("GET", envOrigin+"/done", nil)
		if err == nil && status == 200 {
			var payload struct {
				Done bool `json:"done"`
			}
			if json.Unmarshal(body, &payload) == nil && payload.Done {
				return
			}
		}
		bo.next()
	}
}

// -------------------------------------------------------------------------------------------
// the whole life of the process: bind, announce, wait, shut down
// -------------------------------------------------------------------------------------------

func run() error {
	envOrigin := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envOrigin == "" {
		return fmt.Errorf("MAKEFUN_ENV_URL is not set: there is no env to register with")
	}

	st := store.NewStore()
	r := &httpd.Router{Middleware: dispatcher}
	githttp.Register(r, st)
	mcp.Register(r, st)
	graphql.Register(r, st)
	rest.Register(r, st)
	r.Handle("GET", "/healthz", func(c *httpd.Ctx) httpd.Result {
		return httpd.JSON(200, map[string]any{"ok": true})
	})
	r.NotFound = func(c *httpd.Ctx) httpd.Result {
		return httpd.JSON(404, map[string]any{"message": "Not Found"})
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return err
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return fmt.Errorf("the server did not bind an ephemeral TCP port")
	}
	port := addr.Port

	var shuttingDown atomic.Bool
	serveErr := make(chan error, 1)
	go func() {
		if err := r.Serve(ln); err != nil && !shuttingDown.Load() {
			serveErr <- err
		}
	}()

	fmt.Printf("[entry] listening on port %d\n", port)

	if err := announcePort(envOrigin, port); err != nil {
		_ = ln.Close()
		return err
	}
	fmt.Print("[entry] registered; waiting for the env to say done\n")

	done := make(chan struct{})
	go func() {
		awaitCompletion(envOrigin)
		close(done)
	}()

	select {
	case err := <-serveErr:
		_ = ln.Close()
		return err
	case <-done:
	}
	shuttingDown.Store(true)
	_ = ln.Close()
	fmt.Print("[entry] done\n")
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %s\n", err)
		os.Exit(1)
	}
}
