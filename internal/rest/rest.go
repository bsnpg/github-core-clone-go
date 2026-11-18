// Package rest serves the GitHub REST v3 surface under /api/v3.
//
// This is a port of the Node entry's rest.ts: the same routes, status
// codes, JSON shapes (field names, nesting and order) and headers. Every
// reply is application/json with a non-empty X-GitHub-Request-Id; errors
// carry a JSON body with a string "message"; unknown routes are JSON 404s;
// malformed JSON bodies are 400s; a missing bearer token is a 401; and a
// repository under any owner but the token's is a 404, never a 403.
//
// The single-issue GET serves the issue's cached ViewBuf bytes when they
// exist (the read workload asks for the same issue thousands of times),
// building and caching them on first read; any mutation of the issue clears
// the cache. Handlers run under a package RWMutex because the store's
// methods are not internally locked: pure reads take the read lock, anything
// that can write the store (including pull views, which can fill the flat
// tree cache) takes the write lock.
//
// No wall clock and no randomness here: every timestamp comes from
// util.TrialNowISO, every request id from the dispatcher's Ctx.RequestID
// (falling back to util.NextRequestID, a per-process counter).
package rest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"forge/internal/gitobj"
	"forge/internal/httpd"
	"forge/internal/store"
	"forge/internal/util"
)

// Register wires every /api/v3 route onto r, backed by st. One prefix
// handler per method does the whole dispatch: the router's prefix match
// also covers the bare "/api/v3" path, and routing everything through the
// same dispatch keeps wrong-method answers identical to the Node entry
// (a 404, not the router's 405 for an exact-path method mismatch).
func Register(r *httpd.Router, st *store.Store) {
	h := &handler{store: st}
	r.Handle("GET", "/api/v3/*", h.dispatch)
	r.Handle("POST", "/api/v3/*", h.dispatch)
	r.Handle("PATCH", "/api/v3/*", h.dispatch)
	r.Handle("PUT", "/api/v3/*", h.dispatch)
	r.Handle("DELETE", "/api/v3/*", h.dispatch)
}

type handler struct {
	store *store.Store
	mu    sync.RWMutex
}

// -------------------------------------------------------------------------------------------
// JSON encoding: byte-identical to Node's JSON.stringify for these values.
// -------------------------------------------------------------------------------------------

// marshalJSON renders v the way JSON.stringify would: keys in struct field
// order, no HTML escaping, no trailing newline. encoding/json with
// SetEscapeHTML(false) still spells backspace, form feed and U+2028/2029
// differently from Node, so those spellings are fixed up afterwards.
func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return fixJSONEscapes(b)
}

// fixJSONEscapes rewrites the four spellings where Go's encoder differs
// from JSON.stringify, only inside string literals: \u0008 -> \b,
// \u000c -> \f, \u2028 and \u2029 -> the raw UTF-8 characters.
func fixJSONEscapes(b []byte) []byte {
	if bytes.IndexByte(b, '\\') < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	inString := false
	for i := 0; i < len(b); {
		c := b[i]
		if !inString {
			out = append(out, c)
			if c == '"' {
				inString = true
			}
			i++
			continue
		}
		if c == '\\' && i+1 < len(b) {
			n := b[i+1]
			if n == 'u' && i+5 < len(b) {
				seq := string(b[i+2 : i+6])
				switch seq {
				case "0008":
					out = append(out, '\\', 'b')
					i += 6
					continue
				case "000c":
					out = append(out, '\\', 'f')
					i += 6
					continue
				case "2028":
					out = append(out, 0xe2, 0x80, 0xa8)
					i += 6
					continue
				case "2029":
					out = append(out, 0xe2, 0x80, 0xa9)
					i += 6
					continue
				}
			}
			out = append(out, c, n)
			i += 2
			continue
		}
		out = append(out, c)
		if c == '"' {
			inString = false
		}
		i++
	}
	return out
}

// -------------------------------------------------------------------------------------------
// replies
// -------------------------------------------------------------------------------------------

func (h *handler) requestID(c *httpd.Ctx) string {
	if c.RequestID != "" {
		return c.RequestID
	}
	return util.NextRequestID()
}

func (h *handler) headers(c *httpd.Ctx, extra map[string]string) map[string]string {
	// Header names are lower-case, as the transport expects: it adds
	// x-github-request-id itself when the result does not already carry
	// one, so a mixed-case key here would duplicate it on the wire.
	out := make(map[string]string, len(extra)+1)
	out["x-github-request-id"] = h.requestID(c)
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func (h *handler) json(c *httpd.Ctx, status int, v any, extra map[string]string) httpd.Result {
	return httpd.JSONRaw(status, marshalJSON(v), h.headers(c, extra))
}

func (h *handler) err(c *httpd.Ctx, status int, message string) httpd.Result {
	return h.json(c, status, errorBody{Message: message}, nil)
}

type errorBody struct {
	Message string `json:"message"`
}

// -------------------------------------------------------------------------------------------
// views (field order matches rest.ts exactly)
// -------------------------------------------------------------------------------------------

type userRef struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

func viewerRef(login string) userRef {
	return userRef{Login: login, ID: store.Viewer.ID, Type: "User"}
}

type repoView struct {
	ID            int     `json:"id"`
	Name          string  `json:"name"`
	FullName      string  `json:"full_name"`
	Owner         userRef `json:"owner"`
	Private       bool    `json:"private"`
	Description   string  `json:"description"`
	DefaultBranch string  `json:"default_branch"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
	HTMLURL       string  `json:"html_url"`
	URL           string  `json:"url"`
}

func makeRepoView(repo *store.RepoRecord) repoView {
	return repoView{
		ID:            repo.ID,
		Name:          repo.Name,
		FullName:      repo.Owner + "/" + repo.Name,
		Owner:         viewerRef(repo.Owner),
		Private:       repo.Private,
		Description:   repo.Description,
		DefaultBranch: repo.DefaultBranch,
		CreatedAt:     repo.CreatedAt,
		UpdatedAt:     repo.UpdatedAt,
		HTMLURL:       "https://example.invalid/" + repo.Owner + "/" + repo.Name,
		URL:           "/api/v3/repos/" + repo.Owner + "/" + repo.Name,
	}
}

type labelView struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

func makeLabelView(label *store.LabelRecord) labelView {
	return labelView{Name: label.Name, Color: label.Color, Description: label.Description}
}

// stockLabelOrder is the order createRepo inserts the stock labels, which
// is the order Node's Map (insertion-ordered) returns them in. Custom
// labels are appended after the stock ones, sorted by name, because the
// Go map keeps no insertion order.
var stockLabelOrder = []string{"bug", "enhancement", "documentation", "question", "good first issue", "help wanted"}

func sortedLabels(repo *store.RepoRecord) []labelView {
	out := make([]labelView, 0, len(repo.Labels))
	seen := make(map[string]bool, len(repo.Labels))
	for _, name := range stockLabelOrder {
		if label, ok := repo.Labels[name]; ok {
			out = append(out, makeLabelView(label))
			seen[name] = true
		}
	}
	var rest []string
	for name := range repo.Labels {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	for _, name := range rest {
		out = append(out, makeLabelView(repo.Labels[name]))
	}
	return out
}

type issueView struct {
	ID        int         `json:"id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []labelView `json:"labels"`
	User      userRef     `json:"user"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	Comments  int         `json:"comments"`
	HTMLURL   string      `json:"html_url"`
	URL       string      `json:"url"`
}

func makeIssueView(issue *store.IssueRecord, repo *store.RepoRecord) issueView {
	labels := make([]labelView, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		if label, ok := repo.Labels[name]; ok {
			labels = append(labels, makeLabelView(label))
		} else {
			labels = append(labels, labelView{Name: name, Color: "ededed", Description: ""})
		}
	}
	return issueView{
		ID:        issue.ID,
		Number:    issue.Number,
		Title:     issue.Title,
		Body:      issue.Body,
		State:     issue.State,
		Labels:    labels,
		User:      viewerRef(issue.User),
		CreatedAt: issue.CreatedAt,
		UpdatedAt: issue.UpdatedAt,
		Comments:  len(issue.Comments),
		HTMLURL:   "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
		URL:       "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
	}
}

// issueViewBytes returns the cached single-issue JSON bytes. The caller
// must hold the write lock; it builds and caches them on first read.
func issueViewBytes(issue *store.IssueRecord, repo *store.RepoRecord) []byte {
	if issue.ViewBuf == nil {
		issue.ViewBuf = marshalJSON(makeIssueView(issue, repo))
	}
	return issue.ViewBuf
}

type pullRef struct {
	Ref   string  `json:"ref"`
	SHA   string  `json:"sha"`
	Label string  `json:"label"`
	User  userRef `json:"user"`
}

type pullView struct {
	ID             int     `json:"id"`
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	State          string  `json:"state"`
	Merged         bool    `json:"merged"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSha *string `json:"merge_commit_sha"`
	Mergeable      bool    `json:"mergeable"`
	Head           pullRef `json:"head"`
	Base           pullRef `json:"base"`
	User           userRef `json:"user"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	HTMLURL        string  `json:"html_url"`
	URL            string  `json:"url"`
}

func (h *handler) makePullView(pull *store.PullRecord, repo *store.RepoRecord) pullView {
	head := pullRef{
		Ref:   pull.HeadRef,
		SHA:   pull.HeadSha,
		Label: repo.Owner + ":" + pull.HeadRef,
		User:  viewerRef(repo.Owner),
	}
	base := pullRef{
		Ref:   pull.BaseRef,
		SHA:   pull.BaseSha,
		Label: repo.Owner + ":" + pull.BaseRef,
		User:  viewerRef(repo.Owner),
	}
	mergeable := false
	if pull.State == "open" {
		if baseTip, ok := repo.Refs["refs/heads/"+pull.BaseRef]; ok {
			headTip, ok := repo.Refs["refs/heads/"+pull.HeadRef]
			if !ok {
				headTip = pull.HeadSha
			}
			mergeable = !h.store.MergeConflicts(repo, baseTip, headTip)
		}
	}
	return pullView{
		ID:             pull.ID,
		Number:         pull.Number,
		Title:          pull.Title,
		Body:           pull.Body,
		State:          pull.State,
		Merged:         pull.Merged,
		MergedAt:       pull.MergedAt,
		MergeCommitSha: pull.MergeCommitSha,
		Mergeable:      mergeable,
		Head:           head,
		Base:           base,
		User:           viewerRef(pull.User),
		CreatedAt:      pull.CreatedAt,
		UpdatedAt:      pull.UpdatedAt,
		HTMLURL:        "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pull.Number),
		URL:            "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/pulls/" + strconv.Itoa(pull.Number),
	}
}

type commentView struct {
	ID        int     `json:"id"`
	Body      string  `json:"body"`
	User      userRef `json:"user"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

func makeCommentView(comment *store.CommentRecord) commentView {
	return commentView{
		ID:        comment.ID,
		Body:      comment.Body,
		User:      viewerRef(comment.User),
		CreatedAt: comment.CreatedAt,
		UpdatedAt: comment.CreatedAt,
	}
}

// -------------------------------------------------------------------------------------------
// request parsing helpers
// -------------------------------------------------------------------------------------------

// segments splits a request path on '/', dropping empty parts, the way the
// Node dispatcher builds its segment list.
func splitPath(path string) []string {
	var out []string
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// decodeSegment percent-decodes one path segment, as decodeURIComponent
// does. It reports false on a malformed escape, where Node would throw a
// URIError.
func decodeSegment(value string) (string, bool) {
	if !strings.Contains(value, "%") {
		return value, true
	}
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return "", false
	}
	return decoded, true
}

// queryPairs is a raw query string parsed in order: duplicate keys keep
// their first position and last value, as a JS object built from
// URLSearchParams iteration would.
type queryPairs struct {
	keys []string
	vals map[string]string
}

func parseQuery(raw string) *queryPairs {
	q := &queryPairs{vals: make(map[string]string)}
	if raw == "" {
		return q
	}
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		key, value := part, ""
		if i := strings.IndexByte(part, '='); i >= 0 {
			key, value = part[:i], part[i+1:]
		}
		if k, err := url.QueryUnescape(key); err == nil {
			key = k
		}
		if v, err := url.QueryUnescape(value); err == nil {
			value = v
		}
		if _, ok := q.vals[key]; !ok {
			q.keys = append(q.keys, key)
		}
		q.vals[key] = value
	}
	return q
}

func (q *queryPairs) get(name string) (string, bool) {
	v, ok := q.vals[name]
	return v, ok
}

// nodeNumber mirrors JavaScript's Number() for query strings: surrounding
// whitespace is ignored, "" is 0, anything unparseable is NaN.
func nodeNumber(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

// parseNumber mirrors Number() plus Number.isInteger for issue/pull numbers.
func parseNumber(s string) (int, bool) {
	f := nodeNumber(s)
	if math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f {
		return 0, false
	}
	if f > math.MaxInt32*2 || f < math.MinInt32*2 {
		return 0, false
	}
	return int(f), true
}

func isRecord(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func stringField(body any, field string) (string, bool) {
	m, ok := isRecord(body)
	if !ok {
		return "", false
	}
	v, ok := m[field].(string)
	return v, ok
}

func boolField(body any, field string) (bool, bool) {
	m, ok := isRecord(body)
	if !ok {
		return false, false
	}
	v, ok := m[field].(bool)
	return v, ok
}

func baseURL(c *httpd.Ctx) string {
	host := c.Headers["host"]
	if host == "" {
		host = "localhost"
	}
	return "http://" + host
}

// formEscape percent-encodes one query key or value the way
// URLSearchParams does: alphanumerics and *-._ pass through, space becomes
// +, everything else is %XX over its UTF-8 bytes.
func formEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '*' || ch == '-' || ch == '.' || ch == '_':
			b.WriteByte(ch)
		case ch == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func encodePairs(pairs [][2]string) string {
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(formEscape(p[0]))
		b.WriteByte('=')
		b.WriteString(formEscape(p[1]))
	}
	return b.String()
}

// paginate slices a list into a page and builds GitHub's Link header when
// more pages exist. It mirrors rest.ts exactly, including the query order
// in the links: the request's other parameters in order, then per_page,
// then page.
func (h *handler) paginate(c *httpd.Ctx, path string, all []any) ([]any, map[string]string) {
	q := parseQuery(c.Query)
	perPageRaw := "30"
	if v, ok := q.get("per_page"); ok {
		perPageRaw = v
	}
	perPage := 30
	if f := nodeNumber(perPageRaw); !math.IsNaN(f) {
		perPage = int(math.Min(100, math.Max(1, math.Trunc(f))))
	}
	pageRaw := "1"
	if v, ok := q.get("page"); ok {
		pageRaw = v
	}
	page := 1
	if f := nodeNumber(pageRaw); !math.IsNaN(f) {
		page = int(math.Max(1, math.Trunc(f)))
	}
	start := (page - 1) * perPage
	var slice []any
	if start < len(all) {
		end := start + perPage
		if end > len(all) {
			end = len(all)
		}
		slice = all[start:end]
	} else {
		slice = []any{}
	}
	totalPages := (len(all) + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	keepQuery := func() [][2]string {
		var out [][2]string
		seen := make(map[string]bool)
		for _, k := range q.keys {
			if k == "page" || k == "per_page" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, [2]string{k, q.vals[k]})
		}
		out = append(out, [2]string{"per_page", strconv.Itoa(perPage)})
		return out
	}
	absolute := func(extra [2]string) string {
		pairs := append(append([][2]string{}, keepQuery()...), extra)
		return baseURL(c) + path + "?" + encodePairs(pairs)
	}
	var links []string
	if page < totalPages {
		links = append(links, "<"+absolute([2]string{"page", strconv.Itoa(page + 1)})+`>; rel="next"`)
		links = append(links, "<"+absolute([2]string{"page", strconv.Itoa(totalPages)})+`>; rel="last"`)
	}
	if page > 1 {
		links = append(links, "<"+absolute([2]string{"page", "1"})+`>; rel="first"`)
		links = append(links, "<"+absolute([2]string{"page", strconv.Itoa(page - 1)})+`>; rel="prev"`)
	}
	if len(links) == 0 {
		return slice, nil
	}
	return slice, map[string]string{"link": strings.Join(links, ", ")}
}

// -------------------------------------------------------------------------------------------
// dispatch
// -------------------------------------------------------------------------------------------

// lockKind decides which lock a request runs under. Pure reads take the
// read lock; anything that can write the store — every mutation, and the
// pull views, which can fill the flat-tree cache — takes the write lock.
// The single-issue GET is decided separately so its cached-bytes fast path
// runs under the read lock.
func (h *handler) withRead(fn func() httpd.Result) httpd.Result {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return fn()
}

func (h *handler) withWrite(fn func() httpd.Result) httpd.Result {
	h.mu.Lock()
	defer h.mu.Unlock()
	return fn()
}

func (h *handler) dispatch(c *httpd.Ctx) httpd.Result {
	segs := splitPath(c.Path)
	var rest []string
	if len(segs) >= 2 && segs[0] == "api" && segs[1] == "v3" {
		rest = segs[2:]
	} else {
		return h.err(c, 404, "Not Found")
	}

	// The API root GET is public and never parses a body.
	if len(rest) == 0 && c.Method == "GET" {
		body := struct {
			CurrentUserURL string `json:"current_user_url"`
			RateLimitURL   string `json:"rate_limit_url"`
		}{
			CurrentUserURL: baseURL(c) + "/api/v3/user",
			RateLimitURL:   baseURL(c) + "/api/v3/rate_limit",
		}
		return h.json(c, 200, body, map[string]string{"x-oauth-scopes": "repo, read:org"})
	}

	// Every other route parses the JSON body first: a malformed body is a
	// 400 even before the auth check, as in the Node dispatcher. The
	// Node dispatcher's error bodies carry a trailing newline (its
	// jsonResult appends one), unlike the REST handler's, so this one
	// does too.
	var body any
	if len(c.Body) > 0 {
		if err := json.Unmarshal(c.Body, &body); err != nil {
			return httpd.JSONRaw(400, []byte("{\"message\":\"Invalid JSON\"}\n"), h.headers(c, nil))
		}
	}

	if len(rest) == 0 {
		if !c.Authed {
			return h.err(c, 401, "Requires authentication")
		}
		return h.err(c, 404, "Not Found")
	}

	// Meta is public; everything else needs the token.
	if len(rest) == 1 && rest[0] == "meta" && c.Method == "GET" {
		return h.json(c, 200, struct {
			InstalledVersion string `json:"installed_version"`
		}{InstalledVersion: "1.0.0"}, nil)
	}
	if !c.Authed {
		return h.err(c, 401, "Requires authentication")
	}

	return h.route(c, rest, body)
}

func (h *handler) route(c *httpd.Ctx, segs []string, body any) httpd.Result {
	first := segs[0]

	if first == "user" {
		return h.routeUser(c, segs, body)
	}
	if first == "repos" && len(segs) >= 3 {
		return h.routeRepos(c, segs, body)
	}
	return h.err(c, 404, "Not Found")
}

func (h *handler) routeUser(c *httpd.Ctx, segs []string, body any) httpd.Result {
	if len(segs) == 1 {
		if c.Method == "GET" {
			v := struct {
				Login string `json:"login"`
				ID    int    `json:"id"`
				Name  string `json:"name"`
				Email string `json:"email"`
				Type  string `json:"type"`
			}{Login: store.Viewer.Login, ID: store.Viewer.ID, Name: store.Viewer.Name, Email: store.Viewer.Email, Type: "User"}
			return h.json(c, 200, v, nil)
		}
		return h.err(c, 404, "Not Found")
	}
	if segs[1] != "repos" || len(segs) != 2 {
		return h.err(c, 404, "Not Found")
	}
	switch c.Method {
	case "POST":
		return h.withWrite(func() httpd.Result { return h.createUserRepo(c, body) })
	case "GET":
		return h.withRead(func() httpd.Result {
			repos := h.store.ListRepos(store.Viewer.Login)
			items := make([]any, 0, len(repos))
			for _, repo := range repos {
				items = append(items, makeRepoView(repo))
			}
			page, headers := h.paginate(c, "/api/v3/user/repos", items)
			return h.json(c, 200, page, headers)
		})
	default:
		return h.err(c, 404, "Not Found")
	}
}

func (h *handler) createUserRepo(c *httpd.Ctx, body any) httpd.Result {
	name, ok := stringField(body, "name")
	if !ok || name == "" {
		return h.err(c, 422, "name is required")
	}
	description, _ := stringField(body, "description")
	priv, _ := boolField(body, "private")
	repo := h.store.CreateRepo(store.Viewer.Login, name, description, priv)
	if repo == nil {
		return h.err(c, 422, "Repository already exists")
	}
	return h.json(c, 201, makeRepoView(repo), nil)
}

// namedRepo is the repository a route names, or the 404 result for it: the
// owner must be the viewer, so another owner is a 404, never a 403.
func (h *handler) namedRepo(c *httpd.Ctx, owner, name string) (*store.RepoRecord, httpd.Result, bool) {
	if strings.ToLower(owner) != store.Viewer.Login {
		return nil, h.err(c, 404, "Not Found"), false
	}
	repo := h.store.GetRepo(owner, name)
	if repo == nil {
		return nil, h.err(c, 404, "Not Found"), false
	}
	return repo, httpd.Result{}, true
}

func (h *handler) routeRepos(c *httpd.Ctx, segs []string, body any) httpd.Result {
	owner, ok := decodeSegment(segs[1])
	if !ok {
		return h.err(c, 500, "URI malformed")
	}
	name, ok := decodeSegment(segs[2])
	if !ok {
		return h.err(c, 500, "URI malformed")
	}
	rest := segs[3:]
	if len(rest) == 0 {
		switch c.Method {
		case "GET":
			return h.withRead(func() httpd.Result {
				repo, res, ok := h.namedRepo(c, owner, name)
				if !ok {
					return res
				}
				return h.json(c, 200, makeRepoView(repo), nil)
			})
		case "PATCH":
			return h.withWrite(func() httpd.Result {
				repo, res, ok := h.namedRepo(c, owner, name)
				if !ok {
					return res
				}
				if _, isRec := isRecord(body); isRec {
					if description, ok := stringField(body, "description"); ok {
						repo.Description = description
					}
					if priv, ok := boolField(body, "private"); ok {
						repo.Private = priv
					}
					repo.UpdatedAt = util.TrialNowISO()
				}
				return h.json(c, 200, makeRepoView(repo), nil)
			})
		case "DELETE":
			return h.withWrite(func() httpd.Result {
				_, res, ok := h.namedRepo(c, owner, name)
				if !ok {
					return res
				}
				h.store.DeleteRepo(owner, name)
				return httpd.Bytes(204, []byte{}, "application/json", h.headers(c, nil))
			})
		default:
			return h.err(c, 404, "Not Found")
		}
	}
	switch rest[0] {
	case "branches":
		return h.routeBranches(c, owner, name, rest[1:], body)
	case "commits":
		return h.routeCommits(c, owner, name, body)
	case "git":
		return h.routeGitRefs(c, owner, name, rest[1:], body)
	case "contents":
		return h.routeContents(c, owner, name, rest[1:], body)
	case "labels":
		return h.routeLabels(c, owner, name, body)
	case "issues":
		return h.routeIssues(c, owner, name, rest[1:], body)
	case "pulls":
		return h.routePulls(c, owner, name, rest[1:], body)
	default:
		return h.err(c, 404, "Not Found")
	}
}

func (h *handler) routeBranches(c *httpd.Ctx, owner, name string, rest []string, _ any) httpd.Result {
	if c.Method != "GET" {
		return h.err(c, 404, "Not Found")
	}
	return h.withRead(func() httpd.Result {
		repo, res, ok := h.namedRepo(c, owner, name)
		if !ok {
			return res
		}
		if len(rest) == 0 {
			var names []string
			for ref := range repo.Refs {
				if strings.HasPrefix(ref, "refs/heads/") {
					names = append(names, ref[len("refs/heads/"):])
				}
			}
			sort.Strings(names)
			type branch struct {
				Name   string `json:"name"`
				Commit struct {
					SHA string `json:"sha"`
				} `json:"commit"`
				Protected bool `json:"protected"`
			}
			out := make([]branch, 0, len(names))
			for _, n := range names {
				var b branch
				b.Name = n
				b.Commit.SHA = repo.Refs["refs/heads/"+n]
				b.Protected = false
				out = append(out, b)
			}
			return h.json(c, 200, out, nil)
		}
		branch, ok := decodeSegment(rest[0])
		if !ok {
			return h.err(c, 500, "URI malformed")
		}
		sha, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			return h.err(c, 404, "Branch not found")
		}
		v := struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
			Protected bool `json:"protected"`
		}{Name: branch, Protected: false}
		v.Commit.SHA = sha
		return h.json(c, 200, v, nil)
	})
}

var identPattern = regexp.MustCompile(`^(.*?) <(.*?)> (\d+) ([+-]\d{4})$`)

type identView struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

func makeIdentView(ident string) identView {
	m := identPattern.FindStringSubmatch(ident)
	if m == nil {
		return identView{Name: ident, Email: "", Date: util.TrialNowISO()}
	}
	epoch, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		return identView{Name: m[1], Email: m[2], Date: util.TrialNowISO()}
	}
	return identView{
		Name:  m[1],
		Email: m[2],
		Date:  time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05.000Z"),
	}
}

func (h *handler) routeCommits(c *httpd.Ctx, owner, name string, _ any) httpd.Result {
	if c.Method != "GET" {
		return h.err(c, 404, "Not Found")
	}
	return h.withRead(func() httpd.Result {
		repo, res, ok := h.namedRepo(c, owner, name)
		if !ok {
			return res
		}
		q := parseQuery(c.Query)
		ref := repo.DefaultBranch
		if v, ok := q.get("sha"); ok {
			ref = v
		}
		tip := h.store.ResolveRef(repo, ref)
		if tip == "" {
			return h.err(c, 404, "No commit found for this ref")
		}
		history := h.store.CommitHistory(repo, tip)
		type parentRef struct {
			SHA string `json:"sha"`
		}
		type commitBody struct {
			Message   string    `json:"message"`
			Author    identView `json:"author"`
			Committer identView `json:"committer"`
		}
		type commitEntry struct {
			SHA     string      `json:"sha"`
			Commit  commitBody  `json:"commit"`
			Parents []parentRef `json:"parents"`
		}
		out := make([]commitEntry, 0, len(history))
		for _, e := range history {
			parents := make([]parentRef, 0, len(e.Commit.Parents))
			for _, p := range e.Commit.Parents {
				parents = append(parents, parentRef{SHA: p})
			}
			out = append(out, commitEntry{
				SHA: e.SHA,
				Commit: commitBody{
					Message:   e.Commit.Message,
					Author:    makeIdentView(e.Commit.Author),
					Committer: makeIdentView(e.Commit.Committer),
				},
				Parents: parents,
			})
		}
		return h.json(c, 200, out, nil)
	})
}

func (h *handler) routeGitRefs(c *httpd.Ctx, owner, name string, rest []string, body any) httpd.Result {
	if len(rest) == 0 || rest[0] != "refs" {
		return h.err(c, 404, "Not Found")
	}
	if c.Method == "POST" {
		return h.withWrite(func() httpd.Result {
			return h.gitRefsPost(c, owner, name, body)
		})
	}
	if c.Method == "GET" && len(rest) >= 2 {
		return h.withRead(func() httpd.Result {
			return h.gitRefsGet(c, owner, name, rest)
		})
	}
	return h.err(c, 404, "Not Found")
}

func (h *handler) gitRefsPost(c *httpd.Ctx, owner, name string, body any) httpd.Result {
	repo, res, ok := h.namedRepo(c, owner, name)
	if !ok {
		return res
	}
	ref, ok := stringField(body, "ref")
	sha, okSha := stringField(body, "sha")
	if !ok || !okSha {
		return h.err(c, 422, "ref and sha are required")
	}
	if !strings.HasPrefix(ref, "refs/") {
		return h.err(c, 422, "ref must start with refs/")
	}
	if _, exists := repo.Objects[sha]; !exists {
		return h.err(c, 422, "Object does not exist")
	}
	if _, exists := repo.Refs[ref]; exists {
		return h.err(c, 422, "Reference already exists")
	}
	repo.Refs[ref] = sha
	v := struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}{Ref: ref}
	v.Object.SHA = sha
	v.Object.Type = "commit"
	return h.json(c, 201, v, nil)
}

func (h *handler) gitRefsGet(c *httpd.Ctx, owner, name string, rest []string) httpd.Result {
	repo, res, ok := h.namedRepo(c, owner, name)
	if !ok {
		return res
	}
	parts := make([]string, 0, len(rest)-1)
	for _, p := range rest[1:] {
		dec, ok := decodeSegment(p)
		if !ok {
			return h.err(c, 500, "URI malformed")
		}
		parts = append(parts, dec)
	}
	refName := "refs/" + strings.Join(parts, "/")
	sha, ok := repo.Refs[refName]
	if !ok {
		return h.err(c, 404, "Not Found")
	}
	v := struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}{Ref: refName}
	v.Object.SHA = sha
	v.Object.Type = "commit"
	return h.json(c, 200, v, nil)
}

// -------------------------------------------------------------------------------------------
// contents
// -------------------------------------------------------------------------------------------

// refTipCommit is the commit a ref names, or nil when the repo has no
// commit there. A nil ref means the default branch.
func (h *handler) refTipCommit(repo *store.RepoRecord, ref *string) (sha, tree string, ok bool) {
	var tip string
	if ref == nil {
		tip = repo.Refs["refs/heads/"+repo.DefaultBranch]
	} else {
		tip = h.store.ResolveRef(repo, *ref)
	}
	if tip == "" {
		return "", "", false
	}
	obj := repo.Objects[tip]
	if obj == nil || obj.Type != gitobj.Commit {
		return "", "", false
	}
	return tip, gitobj.ParseCommit(obj.Data).Tree, true
}

func (h *handler) walkPath(repo *store.RepoRecord, treeOID string, segments []string) (oid string, typ gitobj.ObjectType, ok bool) {
	current := treeOID
	for i, seg := range segments {
		obj := repo.Objects[current]
		if obj == nil || obj.Type != gitobj.Tree {
			return "", "", false
		}
		var entry *gitobj.TreeEntry
		for _, cand := range gitobj.ParseTree(obj.Data) {
			if cand.Name == seg {
				e := cand
				entry = &e
				break
			}
		}
		if entry == nil {
			return "", "", false
		}
		if i == len(segments)-1 {
			target := repo.Objects[entry.OID]
			if target == nil {
				return "", "", false
			}
			return entry.OID, target.Type, true
		}
		current = entry.OID
	}
	return "", "", false
}

type dirEntry struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
	Size int    `json:"size"`
}

func (h *handler) dirEntries(repo *store.RepoRecord, treeOID, dirPath string) []dirEntry {
	obj := repo.Objects[treeOID]
	if obj == nil || obj.Type != gitobj.Tree {
		return []dirEntry{}
	}
	out := make([]dirEntry, 0)
	for _, entry := range gitobj.ParseTree(obj.Data) {
		childPath := entry.Name
		if dirPath != "" {
			childPath = dirPath + "/" + entry.Name
		}
		isDir := entry.Mode == "40000"
		size := 0
		if !isDir {
			if child := repo.Objects[entry.OID]; child != nil {
				size = len(child.Data)
			}
		}
		typ := "file"
		if isDir {
			typ = "dir"
		}
		out = append(out, dirEntry{Type: typ, Name: entry.Name, Path: childPath, SHA: entry.OID, Size: size})
	}
	return out
}

func (h *handler) contentsPath(rest []string) (string, bool) {
	parts := make([]string, 0, len(rest))
	for _, p := range rest {
		dec, ok := decodeSegment(p)
		if !ok {
			return "", false
		}
		parts = append(parts, dec)
	}
	return strings.Join(parts, "/"), true
}

func (h *handler) routeContents(c *httpd.Ctx, owner, name string, rest []string, body any) httpd.Result {
	path, ok := h.contentsPath(rest)
	if !ok {
		return h.err(c, 500, "URI malformed")
	}
	switch c.Method {
	case "GET":
		return h.withRead(func() httpd.Result {
			repo, res, ok := h.namedRepo(c, owner, name)
			if !ok {
				return res
			}
			return h.getContents(c, repo, path)
		})
	case "PUT":
		return h.withWrite(func() httpd.Result {
			repo, res, ok := h.namedRepo(c, owner, name)
			if !ok {
				return res
			}
			return h.putContents(c, repo, path, body)
		})
	case "DELETE":
		return h.withWrite(func() httpd.Result {
			repo, res, ok := h.namedRepo(c, owner, name)
			if !ok {
				return res
			}
			return h.deleteContents(c, repo, path, body)
		})
	default:
		return h.err(c, 404, "Not Found")
	}
}

func (h *handler) getContents(c *httpd.Ctx, repo *store.RepoRecord, path string) httpd.Result {
	q := parseQuery(c.Query)
	var ref *string
	if v, ok := q.get("ref"); ok {
		ref = &v
	}
	_, tree, ok := h.refTipCommit(repo, ref)
	if !ok {
		return h.err(c, 404, "Not Found")
	}
	var segments []string
	if path != "" {
		segments = strings.Split(path, "/")
	}
	if len(segments) == 0 {
		return h.json(c, 200, h.dirEntries(repo, tree, ""), nil)
	}
	oid, typ, ok := h.walkPath(repo, tree, segments)
	if !ok {
		return h.err(c, 404, "Not Found")
	}
	if typ == gitobj.Blob {
		blob := repo.Objects[oid]
		if blob == nil {
			return h.err(c, 404, "Not Found")
		}
		v := struct {
			Type     string `json:"type"`
			Encoding string `json:"encoding"`
			Size     int    `json:"size"`
			Name     string `json:"name"`
			Path     string `json:"path"`
			Content  string `json:"content"`
			SHA      string `json:"sha"`
		}{
			Type:     "file",
			Encoding: "base64",
			Size:     len(blob.Data),
			Name:     segments[len(segments)-1],
			Path:     path,
			Content:  util.B64EncodeBytes(blob.Data),
			SHA:      oid,
		}
		return h.json(c, 200, v, nil)
	}
	if typ == gitobj.Tree {
		return h.json(c, 200, h.dirEntries(repo, oid, path), nil)
	}
	return h.err(c, 404, "Not Found")
}

// sanitizeUTF8 converts decoded bytes to a string the way Node's
// Buffer.toString('utf8') does: valid UTF-8 passes through untouched, and
// each invalid byte becomes U+FFFD. (One corner differs: a truncated
// multi-byte sequence at the very end of the input gets one replacement
// per byte here, where Node's decoder emits a single replacement for the
// whole truncated sequence.)
func sanitizeUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			sb.WriteString("\uFFFD")
			b = b[1:]
			continue
		}
		sb.Write(b[:size])
		b = b[size:]
	}
	return sb.String()
}

// decodeContentBase64 decodes a contents body exactly the way Node's
// Buffer.from(s, 'base64') does: characters outside the alphabet are
// skipped, decoding stops at the first padded quad, and a trailing
// partial group decodes (3 chars -> 2 bytes, 2 chars -> 1 byte, 1 char ->
// nothing). Every input decodes to something, possibly empty, so the
// failure branch is unreachable by construction and kept only as defense.
func decodeContentBase64(contentB64 string) ([]byte, bool) {
	var b strings.Builder
	for i := 0; i < len(contentB64); i++ {
		ch := contentB64[i]
		if ch == '=' {
			break
		}
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '+' || ch == '/' {
			b.WriteByte(ch)
		}
	}
	s := b.String()
	if len(s)%4 == 1 {
		s = s[:len(s)-1]
	}
	decoded, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return decoded, true
}

func (h *handler) putContents(c *httpd.Ctx, repo *store.RepoRecord, path string, body any) httpd.Result {
	message := "Update " + path
	if v, ok := stringField(body, "message"); ok {
		message = v
	}
	contentB64, ok := stringField(body, "content")
	if !ok {
		return h.err(c, 422, "content is required")
	}
	branch := repo.DefaultBranch
	if v, ok := stringField(body, "branch"); ok {
		branch = v
	}
	sha, _ := stringField(body, "sha")
	_, tree, hasTip := h.refTipCommit(repo, &branch)
	if !hasTip {
		if _, exists := repo.Refs["refs/heads/"+branch]; exists {
			return h.err(c, 404, "Branch not found")
		}
		if branch != repo.DefaultBranch {
			return h.err(c, 404, "Branch not found")
		}
	}
	segments := strings.Split(path, "/")
	existed := false
	if hasTip {
		_, _, existed = h.walkPath(repo, tree, segments)
	}
	if existed {
		currentOID, _, _ := h.walkPath(repo, tree, segments)
		if _, hasSha := stringField(body, "sha"); !hasSha {
			return h.err(c, 422, "sha is required to update a file")
		}
		if currentOID != sha {
			return h.err(c, 409, "sha does not match the current file")
		}
	}
	decoded, ok := decodeContentBase64(contentB64)
	if !ok {
		return h.err(c, 400, "Invalid base64 content")
	}
	// Node turns the decoded bytes into a JS string and back: invalid
	// UTF-8 becomes U+FFFD, so the stored blob (and its size) is the
	// sanitized form, not the raw bytes.
	content := sanitizeUTF8(decoded)
	created := h.store.ContentCommit(repo, branch, path, &content, message)
	if created == nil {
		return h.err(c, 404, "Branch not found")
	}
	name := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		name = path[i+1:]
	}
	status := 201
	if existed {
		status = 200
	}
	v := struct {
		Content struct {
			Name     string `json:"name"`
			Path     string `json:"path"`
			SHA      string `json:"sha"`
			Size     int    `json:"size"`
			Encoding string `json:"encoding"`
			Type     string `json:"type"`
		} `json:"content"`
		Commit struct {
			SHA     string `json:"sha"`
			Message string `json:"message"`
		} `json:"commit"`
	}{}
	v.Content.Name = name
	v.Content.Path = path
	v.Content.SHA = created.ContentSHA
	v.Content.Size = len(content)
	v.Content.Encoding = "base64"
	v.Content.Type = "file"
	v.Commit.SHA = created.CommitSHA
	v.Commit.Message = message
	return h.json(c, status, v, nil)
}

func (h *handler) deleteContents(c *httpd.Ctx, repo *store.RepoRecord, path string, body any) httpd.Result {
	message := "Delete " + path
	if v, ok := stringField(body, "message"); ok {
		message = v
	}
	branch := repo.DefaultBranch
	if v, ok := stringField(body, "branch"); ok {
		branch = v
	}
	sha, hasSha := stringField(body, "sha")
	_, tree, hasTip := h.refTipCommit(repo, &branch)
	if !hasTip {
		return h.err(c, 404, "Not Found")
	}
	currentOID, _, found := h.walkPath(repo, tree, strings.Split(path, "/"))
	if !found {
		return h.err(c, 404, "Not Found")
	}
	if hasSha && currentOID != sha {
		return h.err(c, 409, "sha does not match the current file")
	}
	deleted := h.store.ContentCommit(repo, branch, path, nil, message)
	if deleted == nil {
		return h.err(c, 404, "Not Found")
	}
	v := struct {
		Commit struct {
			SHA     string `json:"sha"`
			Message string `json:"message"`
		} `json:"commit"`
	}{}
	v.Commit.SHA = deleted.CommitSHA
	v.Commit.Message = message
	return h.json(c, 200, v, nil)
}

// -------------------------------------------------------------------------------------------
// labels
// -------------------------------------------------------------------------------------------

func (h *handler) routeLabels(c *httpd.Ctx, owner, name string, body any) httpd.Result {
	switch c.Method {
	case "POST":
		return h.withWrite(func() httpd.Result {
			repo, res, ok := h.namedRepo(c, owner, name)
			if !ok {
				return res
			}
			labelName, ok := stringField(body, "name")
			if !ok || labelName == "" {
				return h.err(c, 422, "name is required")
			}
			color, _ := stringField(body, "color")
			if color == "" {
				// Node defaults only when absent; an explicit "" is kept by
				// createLabel's `color || 'ededed'` fallback too.
				color = "ededed"
			}
			return h.json(c, 201, makeLabelView(h.store.CreateLabel(repo, labelName, color)), nil)
		})
	case "GET":
		return h.withRead(func() httpd.Result {
			repo, res, ok := h.namedRepo(c, owner, name)
			if !ok {
				return res
			}
			return h.json(c, 200, sortedLabels(repo), nil)
		})
	default:
		return h.err(c, 404, "Not Found")
	}
}

// -------------------------------------------------------------------------------------------
// issues
// -------------------------------------------------------------------------------------------

func (h *handler) routeIssues(c *httpd.Ctx, owner, name string, rest []string, body any) httpd.Result {
	if len(rest) == 0 {
		switch c.Method {
		case "POST":
			return h.withWrite(func() httpd.Result {
				repo, res, ok := h.namedRepo(c, owner, name)
				if !ok {
					return res
				}
				return h.createIssue(c, repo, body)
			})
		case "GET":
			return h.withRead(func() httpd.Result {
				repo, res, ok := h.namedRepo(c, owner, name)
				if !ok {
					return res
				}
				return h.listIssues(c, repo)
			})
		default:
			return h.err(c, 404, "Not Found")
		}
	}
	number, ok := parseNumber(rest[0])
	if !ok {
		return h.err(c, 404, "Not Found")
	}
	if len(rest) >= 2 && rest[1] == "comments" {
		switch c.Method {
		case "POST":
			return h.withWrite(func() httpd.Result {
				repo, res, ok := h.namedRepo(c, owner, name)
				if !ok {
					return res
				}
				issue := h.store.GetIssue(repo, number)
				if issue == nil {
					return h.err(c, 404, "Not Found")
				}
				commentBody, ok := stringField(body, "body")
				if !ok {
					return h.err(c, 422, "body is required")
				}
				return h.json(c, 201, makeCommentView(h.store.CreateComment(repo, issue, commentBody)), nil)
			})
		case "GET":
			return h.withRead(func() httpd.Result {
				repo, res, ok := h.namedRepo(c, owner, name)
				if !ok {
					return res
				}
				issue := h.store.GetIssue(repo, number)
				if issue == nil {
					return h.err(c, 404, "Not Found")
				}
				out := make([]commentView, 0, len(issue.Comments))
				for _, comment := range issue.Comments {
					out = append(out, makeCommentView(comment))
				}
				return h.json(c, 200, out, nil)
			})
		default:
			return h.err(c, 404, "Not Found")
		}
	}
	if len(rest) > 1 {
		return h.err(c, 404, "Not Found")
	}
	switch c.Method {
	case "GET":
		return h.getSingleIssue(c, owner, name, number)
	case "PATCH":
		return h.withWrite(func() httpd.Result {
			repo, res, ok := h.namedRepo(c, owner, name)
			if !ok {
				return res
			}
			issue := h.store.GetIssue(repo, number)
			if issue == nil {
				return h.err(c, 404, "Not Found")
			}
			if _, isRec := isRecord(body); isRec {
				if state, ok := stringField(body, "state"); ok && (state == "open" || state == "closed") {
					issue.State = state
				}
				if title, ok := stringField(body, "title"); ok {
					issue.Title = title
				}
				if b, ok := stringField(body, "body"); ok {
					issue.Body = b
				}
				issue.UpdatedAt = util.TrialNowISO()
				issue.ViewBuf = nil
			}
			return h.json(c, 200, makeIssueView(issue, repo), nil)
		})
	default:
		return h.err(c, 404, "Not Found")
	}
}

// getSingleIssue is the read workload's hot path: under the read lock it
// serves the issue's cached ViewBuf bytes when they exist. On a cache miss
// it drops the read lock, takes the write lock, re-resolves the issue
// (it may have been deleted in between) and builds the cache.
func (h *handler) getSingleIssue(c *httpd.Ctx, owner, name string, number int) httpd.Result {
	h.mu.RLock()
	repo, res, ok := h.namedRepo(c, owner, name)
	if !ok {
		h.mu.RUnlock()
		return res
	}
	issue := h.store.GetIssue(repo, number)
	if issue == nil {
		h.mu.RUnlock()
		return h.err(c, 404, "Not Found")
	}
	if issue.ViewBuf != nil {
		buf := issue.ViewBuf
		h.mu.RUnlock()
		return httpd.JSONRaw(200, buf, h.headers(c, nil))
	}
	h.mu.RUnlock()
	return h.withWrite(func() httpd.Result {
		repo, res, ok := h.namedRepo(c, owner, name)
		if !ok {
			return res
		}
		issue := h.store.GetIssue(repo, number)
		if issue == nil {
			return h.err(c, 404, "Not Found")
		}
		return httpd.JSONRaw(200, issueViewBytes(issue, repo), h.headers(c, nil))
	})
}

func (h *handler) createIssue(c *httpd.Ctx, repo *store.RepoRecord, body any) httpd.Result {
	title, ok := stringField(body, "title")
	if !ok || title == "" {
		return h.err(c, 422, "title is required")
	}
	bodyText, _ := stringField(body, "body")
	var labels []string
	if m, ok := isRecord(body); ok {
		if raw, ok := m["labels"].([]any); ok {
			for _, l := range raw {
				if s, ok := l.(string); ok {
					labels = append(labels, s)
				}
			}
		}
	}
	created, errMsg := h.store.CreateIssue(repo, title, bodyText, labels)
	if errMsg != "" {
		return h.err(c, 422, errMsg)
	}
	return h.json(c, 201, makeIssueView(created, repo), nil)
}

func (h *handler) listIssues(c *httpd.Ctx, repo *store.RepoRecord) httpd.Result {
	q := parseQuery(c.Query)
	state := "open"
	if v, ok := q.get("state"); ok {
		state = v
	}
	sortBy := "created"
	if v, ok := q.get("sort"); ok {
		sortBy = v
	}
	direction := "desc"
	if v, ok := q.get("direction"); ok {
		direction = v
	}
	issues := repo.Issues
	if state != "all" {
		filtered := make([]*store.IssueRecord, 0, len(issues))
		for _, issue := range issues {
			if issue.State == state {
				filtered = append(filtered, issue)
			}
		}
		issues = filtered
	}
	sorted := make([]*store.IssueRecord, len(issues))
	copy(sorted, issues)
	if sortBy == "created" {
		if direction == "asc" {
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })
		} else {
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number > sorted[j].Number })
		}
	}
	items := make([]any, 0, len(sorted))
	for _, issue := range sorted {
		items = append(items, makeIssueView(issue, repo))
	}
	path := "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues"
	page, headers := h.paginate(c, path, items)
	return h.json(c, 200, page, headers)
}

// -------------------------------------------------------------------------------------------
// pull requests
// -------------------------------------------------------------------------------------------

func (h *handler) routePulls(c *httpd.Ctx, owner, name string, rest []string, body any) httpd.Result {
	// Every pull route runs under the write lock: pull views call
	// MergeConflicts, which can fill the flat-tree cache.
	return h.withWrite(func() httpd.Result {
		repo, res, ok := h.namedRepo(c, owner, name)
		if !ok {
			return res
		}
		if len(rest) == 0 {
			switch c.Method {
			case "POST":
				return h.createPull(c, repo, body)
			case "GET":
				return h.listPulls(c, repo)
			default:
				return h.err(c, 404, "Not Found")
			}
		}
		number, ok := parseNumber(rest[0])
		if !ok {
			return h.err(c, 404, "Not Found")
		}
		pull := h.store.GetPull(repo, number)
		if len(rest) >= 2 && rest[1] == "files" {
			if pull == nil {
				return h.err(c, 404, "Not Found")
			}
			if c.Method != "GET" {
				return h.err(c, 404, "Not Found")
			}
			headCommit := repo.Objects[pull.HeadSha]
			baseCommit := repo.Objects[pull.BaseSha]
			if headCommit == nil || baseCommit == nil {
				return h.err(c, 422, "Pull request commits are missing")
			}
			changes := h.store.DiffTrees(repo, gitobj.ParseCommit(baseCommit.Data).Tree, gitobj.ParseCommit(headCommit.Data).Tree)
			type fileChange struct {
				Filename  string `json:"filename"`
				Status    string `json:"status"`
				Additions int    `json:"additions"`
				Deletions int    `json:"deletions"`
				Changes   int    `json:"changes"`
			}
			out := make([]fileChange, 0, len(changes))
			for _, ch := range changes {
				out = append(out, fileChange{
					Filename:  ch.Filename,
					Status:    ch.Status,
					Additions: ch.Additions,
					Deletions: ch.Deletions,
					Changes:   ch.Changes,
				})
			}
			return h.json(c, 200, out, nil)
		}
		if len(rest) >= 2 && rest[1] == "merge" {
			if pull == nil {
				return h.err(c, 404, "Not Found")
			}
			if c.Method != "PUT" {
				return h.err(c, 404, "Not Found")
			}
			sha, errMsg, status := h.store.MergePull(repo, pull)
			if status != 0 {
				return h.err(c, status, errMsg)
			}
			v := struct {
				SHA     string `json:"sha"`
				Merged  bool   `json:"merged"`
				Message string `json:"message"`
			}{SHA: sha, Merged: true, Message: "Pull request #" + strconv.Itoa(pull.Number) + " merged (squash)"}
			return h.json(c, 200, v, nil)
		}
		if len(rest) > 1 {
			return h.err(c, 404, "Not Found")
		}
		if pull == nil {
			return h.err(c, 404, "Not Found")
		}
		if c.Method == "GET" {
			return h.json(c, 200, h.makePullView(pull, repo), nil)
		}
		return h.err(c, 404, "Not Found")
	})
}

func (h *handler) createPull(c *httpd.Ctx, repo *store.RepoRecord, body any) httpd.Result {
	title, okTitle := stringField(body, "title")
	head, okHead := stringField(body, "head")
	base, okBase := stringField(body, "base")
	if !okTitle || !okHead || !okBase {
		return h.err(c, 422, "title, head and base are required")
	}
	bodyText, _ := stringField(body, "body")
	created, errMsg := h.store.CreatePull(repo, title, bodyText, head, base)
	if errMsg != "" {
		return h.err(c, 422, errMsg)
	}
	return h.json(c, 201, h.makePullView(created, repo), nil)
}

func (h *handler) listPulls(c *httpd.Ctx, repo *store.RepoRecord) httpd.Result {
	q := parseQuery(c.Query)
	state := "open"
	if v, ok := q.get("state"); ok {
		state = v
	}
	pulls := repo.Pulls
	if state != "all" {
		filtered := make([]*store.PullRecord, 0, len(pulls))
		for _, pull := range pulls {
			if pull.State == state {
				filtered = append(filtered, pull)
			}
		}
		pulls = filtered
	}
	items := make([]any, 0, len(pulls))
	for _, pull := range pulls {
		items = append(items, h.makePullView(pull, repo))
	}
	path := "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/pulls"
	page, headers := h.paginate(c, path, items)
	return h.json(c, 200, page, headers)
}
