// Package graphql serves the GitHub GraphQL v4 surface at POST /api/graphql.
//
// It is a small, dependency-free executor for the documents gh and
// github-mcp-server actually send: viewer, repository, node(id:), search,
// the __type/__schema probes, and the createIssue / closeIssue /
// createPullRequest / mergePullRequest / addComment mutations.
//
// A GraphQL-level failure is an HTTP 200 whose body carries `errors` (and
// `data: null`); it is not an HTTP error. An unauthenticated request is a
// 401. Two operation documents in one JSON-array body answer two bodies in
// order. Global ids are opaque (R<repo>, I<repo>-<n>, P<repo>-<n>,
// C<...>) and stable across reads; a repository lookup for an unknown
// owner/name answers `data.repository = null` with no errors.
//
// Responses are built as ordered objects so the JSON bytes come out in
// document order, the way the reference implementation emits them.
package graphql

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"forge/internal/httpd"
	"forge/internal/store"
)

// -------------------------------------------------------------------------------------------
// ordered JSON values: objects keep insertion order, like the reference
// -------------------------------------------------------------------------------------------

// jobj is a JSON object whose keys serialize in the order they were set.
type jobj struct {
	keys []string
	vals map[string]any
}

func newObj() *jobj {
	return &jobj{vals: make(map[string]any)}
}

// set stores key, keeping its first position when it is set again.
func (o *jobj) set(key string, value any) *jobj {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = value
	return o
}

// MarshalJSON renders the object with its keys in insertion order.
func (o *jobj) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, o); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeJSON renders one value the way encoding/json would, except that
// strings are not HTML-escaped and objects keep insertion order.
func writeJSON(buf *bytes.Buffer, v any) error {
	switch value := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if value {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case int:
		buf.WriteString(strconv.Itoa(value))
	case int64:
		buf.WriteString(strconv.FormatInt(value, 10))
	case float64:
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		buf.Write(raw)
	case string:
		return writeJSONString(buf, value)
	case []any:
		buf.WriteByte('[')
		for i, item := range value {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSON(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case *jobj:
		buf.WriteByte('{')
		for i, key := range value.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSONString(buf, key); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeJSON(buf, value.vals[key]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSONString(buf, key); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeJSON(buf, value[key]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		buf.Write(raw)
	}
	return nil
}

// writeJSONString encodes one string without HTML escaping, matching the
// reference serializer (which leaves <, > and & alone).
func writeJSONString(buf *bytes.Buffer, s string) error {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
	return nil
}

// marshalJSON renders one response value to bytes.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// -------------------------------------------------------------------------------------------
// a small GraphQL document parser (the subset the suite sends)
// -------------------------------------------------------------------------------------------

type tokenKind int

const (
	tokName tokenKind = iota
	tokString
	tokInt
	tokPunct
	tokEOF
)

type token struct {
	kind  tokenKind
	value string
}

func isSpace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '\f' || ch == '\v' || ch == ','
}

func isDigit(ch byte) bool { return ch >= '0' && ch <= '9' }

func isNameStart(ch byte) bool {
	return ch == '_' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z')
}

func isNameChar(ch byte) bool { return isNameStart(ch) || isDigit(ch) }

// tokenize splits a document into tokens, mirroring the reference lexer:
// commas are insignificant whitespace, # starts a comment, strings take
// backslash escapes, and ... is one token.
func tokenize(document string) ([]token, error) {
	var tokens []token
	i := 0
	for i < len(document) {
		ch := document[i]
		if isSpace(ch) {
			i++
			continue
		}
		if ch == '#' {
			for i < len(document) && document[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '"' {
			var sb strings.Builder
			i++
			for i < len(document) && document[i] != '"' {
				if document[i] == '\\' && i+1 < len(document) {
					esc := document[i+1]
					switch esc {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					default:
						sb.WriteByte(esc)
					}
					i += 2
				} else {
					sb.WriteByte(document[i])
					i++
				}
			}
			i++
			tokens = append(tokens, token{kind: tokString, value: sb.String()})
			continue
		}
		if isDigit(ch) || (ch == '-' && i+1 < len(document) && isDigit(document[i+1])) {
			start := i
			if document[i] == '-' {
				i++
			}
			for i < len(document) && (isDigit(document[i]) || document[i] == '.') {
				i++
			}
			tokens = append(tokens, token{kind: tokInt, value: document[start:i]})
			continue
		}
		if isNameStart(ch) {
			start := i
			for i < len(document) && isNameChar(document[i]) {
				i++
			}
			tokens = append(tokens, token{kind: tokName, value: document[start:i]})
			continue
		}
		if strings.IndexByte("{}()[]:!=@$&|.", ch) >= 0 {
			if ch == '.' && strings.HasPrefix(document[i:], "...") {
				tokens = append(tokens, token{kind: tokPunct, value: "..."})
				i += 3
				continue
			}
			tokens = append(tokens, token{kind: tokPunct, value: string(ch)})
			i++
			continue
		}
		return nil, fmt.Errorf("unexpected character %q in GraphQL document", string(ch))
	}
	tokens = append(tokens, token{kind: tokEOF})
	return tokens, nil
}

type valueKind int

const (
	valVariable valueKind = iota
	valString
	valInt
	valBoolean
	valNull
	valEnum
	valList
	valObject
)

type namedValue struct {
	name  string
	value *gqlValue
}

type gqlValue struct {
	kind   valueKind
	str    string
	num    int
	b      bool
	list   []*gqlValue
	fields []namedValue
}

type gqlField struct {
	alias    string
	hasAlias bool
	name     string
	args     []namedValue
	sels     []selection
}

type selKind int

const (
	selField selKind = iota
	selInlineFragment
	selFragmentSpread
)

type selection struct {
	kind     selKind
	field    *gqlField
	typeCond string
	hasCond  bool
	fragName string
	sels     []selection
}

type operation struct {
	typ     string // "query", "mutation" or "subscription"
	name    string
	hasName bool
	sels    []selection
}

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() token { return p.tokens[p.pos] }
func (p *parser) next() token { t := p.tokens[p.pos]; p.pos++; return t }

func (p *parser) expectPunct(value string) error {
	t := p.next()
	if t.kind != tokPunct || t.value != value {
		return fmt.Errorf("expected %q in GraphQL document", value)
	}
	return nil
}

func (p *parser) acceptPunct(value string) bool {
	t := p.peek()
	if t.kind == tokPunct && t.value == value {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectName() (string, error) {
	t := p.next()
	if t.kind != tokName {
		return "", fmt.Errorf("expected a name in GraphQL document")
	}
	return t.value, nil
}

func (p *parser) parseDocument() ([]*operation, error) {
	var ops []*operation
	for p.peek().kind != tokEOF {
		t := p.peek()
		if t.kind == tokPunct && t.value == "{" {
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			ops = append(ops, &operation{typ: "query", sels: sels})
			continue
		}
		if t.kind == tokName && (t.value == "query" || t.value == "mutation" || t.value == "subscription") {
			typ := t.value
			p.next()
			op := &operation{typ: typ}
			if nxt := p.peek(); nxt.kind == tokName {
				op.name = p.next().value
				op.hasName = true
			}
			// Variable definitions, if any: ( $x: Type = default, ... ) — skipped.
			if p.acceptPunct("(") {
				depth := 1
				for depth > 0 {
					tok := p.next()
					if tok.kind == tokEOF {
						return nil, fmt.Errorf("unterminated variable definitions")
					}
					if tok.kind == tokPunct && tok.value == "(" {
						depth++
					}
					if tok.kind == tokPunct && tok.value == ")" {
						depth--
					}
				}
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			op.sels = sels
			ops = append(ops, op)
			continue
		}
		if t.kind == tokName && t.value == "fragment" {
			p.next()
			if _, err := p.expectName(); err != nil {
				return nil, err
			}
			if nxt := p.peek(); nxt.kind == tokName && nxt.value == "on" {
				p.next()
			}
			if _, err := p.expectName(); err != nil {
				return nil, err
			}
			if _, err := p.parseSelectionSet(); err != nil {
				return nil, err
			}
			continue
		}
		raw, _ := json.Marshal(struct {
			Kind  string `json:"kind"`
			Value string `json:"value"`
		}{kindName(t.kind), t.value})
		return nil, fmt.Errorf("unexpected token %s in GraphQL document", raw)
	}
	return ops, nil
}

func kindName(k tokenKind) string {
	switch k {
	case tokName:
		return "name"
	case tokString:
		return "string"
	case tokInt:
		return "int"
	case tokPunct:
		return "punct"
	default:
		return "eof"
	}
}

func (p *parser) parseSelectionSet() ([]selection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sels []selection
	for !(p.peek().kind == tokPunct && p.peek().value == "}") {
		if p.peek().kind == tokEOF {
			return nil, fmt.Errorf("unterminated selection set")
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return sels, nil
}

func (p *parser) parseSelection() (selection, error) {
	t := p.peek()
	if t.kind == tokPunct && t.value == "..." {
		p.next()
		nxt := p.peek()
		if nxt.kind == tokName && nxt.value == "on" {
			p.next()
			cond, err := p.expectName()
			if err != nil {
				return selection{}, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return selection{}, err
			}
			return selection{kind: selInlineFragment, typeCond: cond, hasCond: true, sels: sels}, nil
		}
		if nxt.kind == tokName {
			name := p.next().value
			return selection{kind: selFragmentSpread, fragName: name}, nil
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return selection{}, err
		}
		return selection{kind: selInlineFragment, sels: sels}, nil
	}
	name, err := p.expectName()
	if err != nil {
		return selection{}, err
	}
	field := &gqlField{name: name}
	if p.acceptPunct(":") {
		field.alias = name
		field.hasAlias = true
		realName, err := p.expectName()
		if err != nil {
			return selection{}, err
		}
		field.name = realName
	}
	if p.acceptPunct("(") {
		for !(p.peek().kind == tokPunct && p.peek().value == ")") {
			argName, err := p.expectName()
			if err != nil {
				return selection{}, err
			}
			if err := p.expectPunct(":"); err != nil {
				return selection{}, err
			}
			val, err := p.parseValue()
			if err != nil {
				return selection{}, err
			}
			field.args = append(field.args, namedValue{name: argName, value: val})
		}
		if err := p.expectPunct(")"); err != nil {
			return selection{}, err
		}
	}
	if p.peek().kind == tokPunct && p.peek().value == "{" {
		sels, err := p.parseSelectionSet()
		if err != nil {
			return selection{}, err
		}
		field.sels = sels
	}
	return selection{kind: selField, field: field}, nil
}

func (p *parser) parseValue() (*gqlValue, error) {
	t := p.next()
	switch t.kind {
	case tokString:
		return &gqlValue{kind: valString, str: t.value}, nil
	case tokInt:
		num, err := strconv.Atoi(t.value)
		if err != nil {
			// A dotted literal is not an integer the executor can use; keep
			// the reference behaviour of treating it as a number.
			f, ferr := strconv.ParseFloat(t.value, 64)
			if ferr != nil {
				return nil, fmt.Errorf("unexpected value in GraphQL document")
			}
			return &gqlValue{kind: valInt, num: int(f)}, nil
		}
		return &gqlValue{kind: valInt, num: num}, nil
	case tokPunct:
		switch t.value {
		case "$":
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			return &gqlValue{kind: valVariable, str: name}, nil
		case "[":
			v := &gqlValue{kind: valList}
			for !(p.peek().kind == tokPunct && p.peek().value == "]") {
				item, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.list = append(v.list, item)
			}
			if err := p.expectPunct("]"); err != nil {
				return nil, err
			}
			return v, nil
		case "{":
			v := &gqlValue{kind: valObject}
			for !(p.peek().kind == tokPunct && p.peek().value == "}") {
				name, err := p.expectName()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				val, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.fields = append(v.fields, namedValue{name: name, value: val})
			}
			if err := p.expectPunct("}"); err != nil {
				return nil, err
			}
			return v, nil
		}
	case tokName:
		switch t.value {
		case "true":
			return &gqlValue{kind: valBoolean, b: true}, nil
		case "false":
			return &gqlValue{kind: valBoolean, b: false}, nil
		case "null":
			return &gqlValue{kind: valNull}, nil
		default:
			return &gqlValue{kind: valEnum, str: t.value}, nil
		}
	}
	return nil, fmt.Errorf("unexpected value in GraphQL document")
}

// -------------------------------------------------------------------------------------------
// the schema the executor knows (field names only — the introspection probes read these)
// -------------------------------------------------------------------------------------------

var schemaFields = map[string][]string{
	"Query":                            {"viewer", "repository", "node", "search", "__type", "__schema"},
	"Mutation":                         {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"User":                             {"login", "id", "name", "email"},
	"Repository":                       {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	"Issue":                            {"id", "number", "title", "state", "body", "user", "createdAt", "updatedAt"},
	"IssueConnection":                  {"nodes", "pageInfo", "totalCount"},
	"PullRequest":                      {"id", "number", "title", "body", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "user"},
	"PullRequestConnection":            {"nodes", "pageInfo", "totalCount"},
	"PullRequestChangedFile":           {"path", "additions", "deletions", "changeType"},
	"PullRequestChangedFileConnection": {"nodes", "pageInfo", "totalCount"},
	"PageInfo":                         {"hasNextPage", "hasPreviousPage", "endCursor", "startCursor"},
	"Ref":                              {"name"},
	"Node":                             {"id"},
	"__Type":                           {"name", "kind", "fields", "enumValues", "description"},
	"__Field":                          {"name", "description", "type"},
	"__EnumValue":                      {"name", "description"},
	"__Schema":                         {"types", "queryType", "mutationType"},
}

var enumValues = map[string][]string{
	"SearchType":             {"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE", "MILESTONE", "ORGANIZATION", "PROJECT", "TEAM", "WIKI", "TOPIC"},
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":         {"ASC", "DESC"},
}

var typeKinds = map[string]string{
	"Query": "OBJECT", "Mutation": "OBJECT", "User": "OBJECT",
	"Repository": "OBJECT", "Issue": "OBJECT", "IssueConnection": "OBJECT",
	"PullRequest": "OBJECT", "PullRequestConnection": "OBJECT",
	"PullRequestChangedFile": "OBJECT", "PageInfo": "OBJECT", "Ref": "OBJECT",
	"Node":       "INTERFACE",
	"SearchType": "ENUM", "IssueState": "ENUM", "PullRequestState": "ENUM",
	"MergeableState": "ENUM", "PullRequestMergeMethod": "ENUM",
	"RepositoryPermission": "ENUM", "IssueOrderField": "ENUM", "OrderDirection": "ENUM",
	"__Type": "OBJECT", "__Field": "OBJECT", "__EnumValue": "OBJECT", "__Schema": "OBJECT",
	"String": "SCALAR", "Int": "SCALAR", "ID": "SCALAR", "Boolean": "SCALAR",
}

// -------------------------------------------------------------------------------------------
// the executor
// -------------------------------------------------------------------------------------------

// gqlError is a GraphQL-level failure: the operation answers data null with
// one error carrying this message, still over HTTP 200.
type gqlError struct{ msg string }

func (e *gqlError) Error() string { return e.msg }

func errf(format string, args ...any) *gqlError {
	return &gqlError{msg: fmt.Sprintf(format, args...)}
}

func isRecord(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// resolveValue resolves a parsed literal against the operation's variables.
func resolveValue(v *gqlValue, vars map[string]any) any {
	switch v.kind {
	case valVariable:
		return vars[v.str]
	case valString, valEnum:
		return v.str
	case valInt:
		return v.num
	case valBoolean:
		return v.b
	case valNull:
		return nil
	case valList:
		out := make([]any, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, resolveValue(item, vars))
		}
		return out
	case valObject:
		out := make(map[string]any, len(v.fields))
		for _, f := range v.fields {
			out[f.name] = resolveValue(f.value, vars)
		}
		return out
	}
	return nil
}

// argValue resolves one named argument of a field, or nil when absent.
func argValue(f *gqlField, name string, vars map[string]any) any {
	for _, a := range f.args {
		if a.name == name {
			return resolveValue(a.value, vars)
		}
	}
	return nil
}

// asInt reads an integer argument, accepting the JSON numbers (float64)
// variables decode to as well as plain ints.
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == math.Trunc(n) && !math.IsInf(n, 0) && !math.IsNaN(n) {
			return int(n), true
		}
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

type rtKind int

const (
	rtUser rtKind = iota
	rtRepo
	rtIssue
	rtPull
	rtIssuesConn
	rtPullsConn
	rtFilesConn
	rtRef
	rtPageInfo
	rtType
	rtSchema
	rtFileChange
)

// rtVal is one value the executor walks: a record, a connection, or a
// schema-describing value.
type rtVal struct {
	kind      rtKind
	repo      *store.RepoRecord
	issue     *store.IssueRecord
	pull      *store.PullRecord
	issues    []*store.IssueRecord
	pulls     []*store.PullRecord
	hasNext   bool
	endCursor string // "" means null
	refName   string
	typeName  string
	file      store.FileChange
}

func typeNameOf(v *rtVal) string {
	if v == nil {
		return "Scalar"
	}
	switch v.kind {
	case rtUser:
		return "User"
	case rtRepo:
		return "Repository"
	case rtIssue:
		return "Issue"
	case rtPull:
		return "PullRequest"
	case rtIssuesConn:
		return "IssueConnection"
	case rtPullsConn:
		return "PullRequestConnection"
	case rtFilesConn:
		return "PullRequestChangedFileConnection"
	case rtRef:
		return "Ref"
	case rtPageInfo:
		return "PageInfo"
	case rtType:
		return "__Type"
	case rtSchema:
		return "__Schema"
	}
	return "Scalar"
}

// issueCursor is the opaque cursor for an issue number.
func issueCursor(number int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", number)))
}

func cursorNumber(cursor any) (int, bool) {
	s, ok := cursor.(string)
	if !ok || s == "" {
		return 0, false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			return 0, false
		}
	}
	if !strings.HasPrefix(string(raw), "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(string(raw), "cursor:"))
	if err != nil {
		return 0, false
	}
	return n, true
}

// issueConnection filters, sorts and pages a repository's issues the way
// the suite's queries ask.
func issueConnection(st *store.Store, repo *store.RepoRecord, field *gqlField, vars map[string]any) *rtVal {
	issues := make([]*store.IssueRecord, len(repo.Issues))
	copy(issues, repo.Issues)
	if states, ok := argValue(field, "states", vars).([]any); ok && len(states) > 0 {
		wanted := make(map[string]bool, len(states))
		for _, s := range states {
			wanted[fmt.Sprint(s)] = true
		}
		kept := issues[:0]
		for _, issue := range issues {
			state := "CLOSED"
			if issue.State == "open" {
				state = "OPEN"
			}
			if wanted[state] {
				kept = append(kept, issue)
			}
		}
		issues = kept
	}
	direction := "ASC"
	if orderBy, ok := argValue(field, "orderBy", vars).(map[string]any); ok {
		if orderBy["direction"] == "DESC" {
			direction = "DESC"
		}
	}
	sort.SliceStable(issues, func(i, j int) bool {
		if direction == "ASC" {
			return issues[i].Number < issues[j].Number
		}
		return issues[i].Number > issues[j].Number
	})
	if afterNumber, ok := cursorNumber(argValue(field, "after", vars)); ok {
		for i, issue := range issues {
			if issue.Number == afterNumber {
				issues = issues[i+1:]
				break
			}
		}
	}
	page := issues
	if first, ok := asInt(argValue(field, "first", vars)); ok {
		if first < 0 {
			first = 0
		}
		if first < len(issues) {
			page = issues[:first]
		}
	}
	hasNext := len(page) > 0 && len(page) < len(issues)
	endCursor := ""
	if len(page) > 0 {
		endCursor = issueCursor(page[len(page)-1].Number)
	}
	return &rtVal{kind: rtIssuesConn, repo: repo, issues: page, hasNext: hasNext, endCursor: endCursor}
}

func pullState(pull *store.PullRecord) string {
	if pull.State == "open" {
		return "OPEN"
	}
	if pull.Merged {
		return "MERGED"
	}
	return "CLOSED"
}

func mergeableState(st *store.Store, repo *store.RepoRecord, pull *store.PullRecord) string {
	if pull.State != "open" {
		return "UNKNOWN"
	}
	baseTip, ok := repo.Refs["refs/heads/"+pull.BaseRef]
	if !ok {
		return "UNKNOWN"
	}
	headTip, ok := repo.Refs["refs/heads/"+pull.HeadRef]
	if !ok {
		headTip = pull.HeadSha
	}
	if st.MergeConflicts(repo, baseTip, headTip) {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

type executor struct {
	st   *store.Store
	vars map[string]any
}

func (e *executor) executeOperation(op *operation) (*jobj, error) {
	rootType := "Query"
	if op.typ == "mutation" {
		rootType = "Mutation"
	}
	return e.executeSelectionSet(op.sels, rootType, nil)
}

func (e *executor) executeSelectionSet(sels []selection, parentType string, value *rtVal) (*jobj, error) {
	out := newObj()
	for _, sel := range sels {
		switch sel.kind {
		case selFragmentSpread:
			continue
		case selInlineFragment:
			if sel.hasCond && sel.typeCond != parentType && sel.typeCond != "Node" {
				continue
			}
			sub, err := e.executeSelectionSet(sel.sels, parentType, value)
			if err != nil {
				return nil, err
			}
			for _, key := range sub.keys {
				out.set(key, sub.vals[key])
			}
			continue
		}
		field := sel.field
		responseName := field.name
		if field.hasAlias {
			responseName = field.alias
		}
		result, err := e.executeField(field, parentType, value)
		if err != nil {
			return nil, err
		}
		out.set(responseName, result)
	}
	return out, nil
}

func (e *executor) executeField(field *gqlField, parentType string, value *rtVal) (any, error) {
	allowed, ok := schemaFields[parentType]
	if !ok {
		return nil, errf("Cannot query field %q on type %q.", field.name, parentType)
	}
	found := false
	for _, name := range allowed {
		if name == field.name {
			found = true
			break
		}
	}
	if !found {
		return nil, errf("Cannot query field %q on type %q.", field.name, parentType)
	}
	switch parentType {
	case "Query":
		return e.queryField(field)
	case "Mutation":
		return e.mutationField(field)
	case "__Type":
		return e.typeField(field, value)
	case "__Schema":
		return e.schemaField(field)
	case "Repository":
		return e.repositoryField(field, value)
	case "Issue":
		return e.issueField(field, value)
	case "PullRequest":
		return e.pullField(field, value)
	case "User":
		return e.userField(field)
	case "IssueConnection", "PullRequestConnection":
		return e.connectionField(field, value)
	case "PullRequestChangedFileConnection":
		return e.filesConnectionField(field, value)
	case "PageInfo":
		return e.pageInfoField(field, value)
	case "Ref":
		return value.refName, nil
	case "Node":
		if field.name == "id" {
			return e.nodeID(value), nil
		}
		return nil, nil
	case "PullRequestChangedFile":
		return e.changedFileField(field, value)
	}
	return nil, nil
}

func (e *executor) nodeID(value *rtVal) any {
	if value == nil {
		return nil
	}
	switch value.kind {
	case rtRepo:
		return e.st.RepoGlobalID(value.repo)
	case rtIssue:
		return e.st.IssueGlobalID(value.repo, value.issue)
	case rtPull:
		return e.st.PullGlobalID(value.repo, value.pull)
	}
	return nil
}

func (e *executor) queryField(field *gqlField) (any, error) {
	switch field.name {
	case "viewer":
		return e.executeSelectionSet(field.sels, "User", &rtVal{kind: rtUser})
	case "repository":
		owner, ok := asString(argValue(field, "owner", e.vars))
		if !ok {
			return nil, nil
		}
		name, ok := asString(argValue(field, "name", e.vars))
		if !ok {
			return nil, nil
		}
		repo := e.st.GetRepo(owner, name)
		if repo == nil {
			return nil, nil
		}
		return e.executeSelectionSet(field.sels, "Repository", &rtVal{kind: rtRepo, repo: repo})
	case "node":
		id, ok := asString(argValue(field, "id", e.vars))
		if !ok {
			return nil, nil
		}
		decoded := e.st.DecodeGlobalID(id)
		if decoded == nil {
			return nil, nil
		}
		var runtime *rtVal
		var typeName string
		switch decoded.Kind {
		case "repo":
			runtime = &rtVal{kind: rtRepo, repo: decoded.Repo}
			typeName = "Repository"
		case "issue":
			runtime = &rtVal{kind: rtIssue, repo: decoded.Repo, issue: decoded.Issue}
			typeName = "Issue"
		case "pull":
			runtime = &rtVal{kind: rtPull, repo: decoded.Repo, pull: decoded.Pull}
			typeName = "PullRequest"
		default:
			return nil, nil
		}
		// `node` is the Node interface: `id` reads directly, the rest
		// comes through inline fragments on the concrete type.
		out := newObj()
		for _, sel := range field.sels {
			switch sel.kind {
			case selFragmentSpread:
				continue
			case selInlineFragment:
				if sel.hasCond && sel.typeCond != typeName {
					continue
				}
				sub, err := e.executeSelectionSet(sel.sels, typeName, runtime)
				if err != nil {
					return nil, err
				}
				for _, key := range sub.keys {
					out.set(key, sub.vals[key])
				}
				continue
			}
			sub := sel.field
			name := sub.name
			if sub.hasAlias {
				name = sub.alias
			}
			if sub.name == "id" {
				out.set(name, e.nodeID(runtime))
				continue
			}
			result, err := e.executeField(sub, typeName, runtime)
			if err != nil {
				return nil, err
			}
			out.set(name, result)
		}
		return out, nil
	case "search":
		query, _ := asString(argValue(field, "query", e.vars))
		typ, _ := asString(argValue(field, "type", e.vars))
		needle := strings.ToLower(query)
		var matches []*rtVal
		if typ == "" || typ == "ISSUE" {
			for _, repo := range e.st.ListRepos(store.Viewer.Login) {
				for _, issue := range repo.Issues {
					if strings.Contains(strings.ToLower(issue.Title), needle) ||
						strings.Contains(strings.ToLower(issue.Body), needle) {
						matches = append(matches, &rtVal{kind: rtIssue, repo: repo, issue: issue})
					}
				}
			}
		}
		sliced := matches
		if first, ok := asInt(argValue(field, "first", e.vars)); ok {
			if first < 0 {
				first = 0
			}
			if first < len(matches) {
				sliced = matches[:first]
			}
		}
		out := newObj()
		for _, sel := range field.sels {
			if sel.kind != selField {
				continue
			}
			sub := sel.field
			name := sub.name
			if sub.hasAlias {
				name = sub.alias
			}
			switch sub.name {
			case "nodes":
				nodes := make([]any, 0, len(sliced))
				for _, entry := range sliced {
					node, err := e.executeSelectionSet(sub.sels, "Issue", entry)
					if err != nil {
						return nil, err
					}
					nodes = append(nodes, node)
				}
				out.set(name, nodes)
			case "pageInfo":
				info, err := e.executeSelectionSet(sub.sels, "PageInfo", &rtVal{kind: rtPageInfo})
				if err != nil {
					return nil, err
				}
				out.set(name, info)
			case "issueCount":
				out.set(name, len(matches))
			}
		}
		return out, nil
	case "__type":
		name, ok := asString(argValue(field, "name", e.vars))
		if !ok {
			return nil, nil
		}
		if _, ok := schemaFields[name]; !ok {
			if _, ok := enumValues[name]; !ok {
				if _, ok := typeKinds[name]; !ok {
					return nil, nil
				}
			}
		}
		return e.executeSelectionSet(field.sels, "__Type", &rtVal{kind: rtType, typeName: name})
	case "__schema":
		return e.executeSelectionSet(field.sels, "__Schema", &rtVal{kind: rtSchema})
	}
	return nil, nil
}

func (e *executor) typeField(field *gqlField, value *rtVal) (any, error) {
	name := value.typeName
	switch field.name {
	case "name":
		return name, nil
	case "kind":
		if kind, ok := typeKinds[name]; ok {
			return kind, nil
		}
		return "OBJECT", nil
	case "description":
		return nil, nil
	case "fields":
		names := schemaFields[name]
		rows := make([]any, 0, len(names))
		for _, fieldName := range names {
			row := newObj()
			for _, sel := range field.sels {
				if sel.kind != selField {
					continue
				}
				sub := sel.field
				key := sub.name
				if sub.hasAlias {
					key = sub.alias
				}
				switch sub.name {
				case "name":
					row.set(key, fieldName)
				case "description":
					row.set(key, nil)
				case "type":
					row.set(key, newObj().set("name", "String").set("kind", "SCALAR"))
				}
			}
			rows = append(rows, row)
		}
		return rows, nil
	case "enumValues":
		values := enumValues[name]
		rows := make([]any, 0, len(values))
		for _, enumName := range values {
			row := newObj()
			for _, sel := range field.sels {
				if sel.kind != selField {
					continue
				}
				sub := sel.field
				key := sub.name
				if sub.hasAlias {
					key = sub.alias
				}
				switch sub.name {
				case "name":
					row.set(key, enumName)
				case "description":
					row.set(key, nil)
				}
			}
			rows = append(rows, row)
		}
		return rows, nil
	}
	return nil, nil
}

func (e *executor) schemaField(field *gqlField) (any, error) {
	switch field.name {
	case "queryType":
		return newObj().set("name", "Query"), nil
	case "mutationType":
		return newObj().set("name", "Mutation"), nil
	case "types":
		names := make([]string, 0, len(typeKinds))
		for name := range typeKinds {
			names = append(names, name)
		}
		sort.Strings(names)
		rows := make([]any, 0, len(names))
		for _, typeName := range names {
			row := newObj()
			for _, sel := range field.sels {
				if sel.kind != selField {
					continue
				}
				sub := sel.field
				key := sub.name
				if sub.hasAlias {
					key = sub.alias
				}
				switch sub.name {
				case "name":
					row.set(key, typeName)
				case "kind":
					row.set(key, typeKinds[typeName])
				case "description":
					row.set(key, nil)
				}
			}
			rows = append(rows, row)
		}
		return rows, nil
	}
	return nil, nil
}

func (e *executor) repositoryField(field *gqlField, value *rtVal) (any, error) {
	repo := value.repo
	switch field.name {
	case "id":
		return e.st.RepoGlobalID(repo), nil
	case "databaseId":
		return repo.ID, nil
	case "name":
		return repo.Name, nil
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name, nil
	case "owner":
		return e.executeSelectionSet(field.sels, "User", &rtVal{kind: rtUser})
	case "defaultBranchRef":
		if _, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; !ok {
			return nil, nil
		}
		return e.executeSelectionSet(field.sels, "Ref", &rtVal{kind: rtRef, refName: repo.DefaultBranch})
	case "viewerPermission":
		return "ADMIN", nil
	case "hasIssuesEnabled":
		return true, nil
	case "issue":
		number, ok := asInt(argValue(field, "number", e.vars))
		if !ok {
			return nil, nil
		}
		issue := e.st.GetIssue(repo, number)
		if issue == nil {
			return nil, nil
		}
		return e.executeSelectionSet(field.sels, "Issue", &rtVal{kind: rtIssue, repo: repo, issue: issue})
	case "issues":
		conn := issueConnection(e.st, repo, field, e.vars)
		return e.executeSelectionSet(field.sels, "IssueConnection", conn)
	case "pullRequests":
		pulls := make([]*store.PullRecord, len(repo.Pulls))
		copy(pulls, repo.Pulls)
		if first, ok := asInt(argValue(field, "first", e.vars)); ok {
			if first < 0 {
				first = 0
			}
			if first < len(pulls) {
				pulls = pulls[:first]
			}
		}
		return e.executeSelectionSet(field.sels, "PullRequestConnection", &rtVal{kind: rtPullsConn, repo: repo, pulls: pulls})
	case "pullRequest":
		number, ok := asInt(argValue(field, "number", e.vars))
		if !ok {
			return nil, nil
		}
		pull := e.st.GetPull(repo, number)
		if pull == nil {
			return nil, nil
		}
		return e.executeSelectionSet(field.sels, "PullRequest", &rtVal{kind: rtPull, repo: repo, pull: pull})
	}
	return nil, nil
}

func (e *executor) issueField(field *gqlField, value *rtVal) (any, error) {
	issue := value.issue
	switch field.name {
	case "id":
		return e.st.IssueGlobalID(value.repo, issue), nil
	case "number":
		return issue.Number, nil
	case "title":
		return issue.Title, nil
	case "state":
		if issue.State == "open" {
			return "OPEN", nil
		}
		return "CLOSED", nil
	case "body":
		return issue.Body, nil
	case "user":
		return e.executeSelectionSet(field.sels, "User", &rtVal{kind: rtUser})
	case "createdAt":
		return issue.CreatedAt, nil
	case "updatedAt":
		return issue.UpdatedAt, nil
	}
	return nil, nil
}

func (e *executor) pullField(field *gqlField, value *rtVal) (any, error) {
	pull := value.pull
	switch field.name {
	case "id":
		return e.st.PullGlobalID(value.repo, pull), nil
	case "number":
		return pull.Number, nil
	case "title":
		return pull.Title, nil
	case "body":
		return pull.Body, nil
	case "state":
		return pullState(pull), nil
	case "merged":
		return pull.Merged, nil
	case "headRefName":
		return pull.HeadRef, nil
	case "baseRefName":
		return pull.BaseRef, nil
	case "mergeable":
		return mergeableState(e.st, value.repo, pull), nil
	case "files":
		return e.executeSelectionSet(field.sels, "PullRequestChangedFileConnection", &rtVal{kind: rtFilesConn, repo: value.repo, pull: pull})
	case "user":
		return e.executeSelectionSet(field.sels, "User", &rtVal{kind: rtUser})
	}
	return nil, nil
}

func (e *executor) userField(field *gqlField) (any, error) {
	switch field.name {
	case "login":
		return store.Viewer.Login, nil
	case "id":
		return store.Viewer.ID, nil
	case "name":
		return store.Viewer.Name, nil
	case "email":
		return store.Viewer.Email, nil
	}
	return nil, nil
}

func (e *executor) connectionField(field *gqlField, value *rtVal) (any, error) {
	switch field.name {
	case "nodes":
		if value.kind == rtIssuesConn {
			nodes := make([]any, 0, len(value.issues))
			for _, issue := range value.issues {
				node, err := e.executeSelectionSet(field.sels, "Issue", &rtVal{kind: rtIssue, repo: value.repo, issue: issue})
				if err != nil {
					return nil, err
				}
				nodes = append(nodes, node)
			}
			return nodes, nil
		}
		if value.kind == rtPullsConn {
			nodes := make([]any, 0, len(value.pulls))
			for _, pull := range value.pulls {
				node, err := e.executeSelectionSet(field.sels, "PullRequest", &rtVal{kind: rtPull, repo: value.repo, pull: pull})
				if err != nil {
					return nil, err
				}
				nodes = append(nodes, node)
			}
			return nodes, nil
		}
		return []any{}, nil
	case "pageInfo":
		hasNext := false
		endCursor := ""
		if value.kind == rtIssuesConn {
			hasNext = value.hasNext
			endCursor = value.endCursor
		}
		return e.executeSelectionSet(field.sels, "PageInfo", &rtVal{kind: rtPageInfo, hasNext: hasNext, endCursor: endCursor})
	case "totalCount":
		if value.kind == rtIssuesConn {
			return len(value.repo.Issues), nil
		}
		if value.kind == rtPullsConn {
			return len(value.repo.Pulls), nil
		}
		return 0, nil
	}
	return nil, nil
}

func (e *executor) filesConnectionField(field *gqlField, value *rtVal) (any, error) {
	if field.name != "nodes" {
		return nil, nil
	}
	if value == nil || value.kind != rtFilesConn {
		return []any{}, nil
	}
	repo, pull := value.repo, value.pull
	headHist := e.st.CommitHistory(repo, pull.HeadSha)
	baseHist := e.st.CommitHistory(repo, pull.BaseSha)
	if len(headHist) == 0 || len(baseHist) == 0 {
		return []any{}, nil
	}
	changes := e.st.DiffTrees(repo, baseHist[0].Commit.Tree, headHist[0].Commit.Tree)
	if first, ok := asInt(argValue(field, "first", e.vars)); ok {
		if first < 0 {
			first = 0
		}
		if first < len(changes) {
			changes = changes[:first]
		}
	}
	nodes := make([]any, 0, len(changes))
	for _, change := range changes {
		node, err := e.executeSelectionSet(field.sels, "PullRequestChangedFile", &rtVal{kind: rtFileChange, file: change})
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

func (e *executor) changedFileField(field *gqlField, value *rtVal) (any, error) {
	change := value.file
	switch field.name {
	case "path":
		return change.Filename, nil
	case "additions":
		return change.Additions, nil
	case "deletions":
		return change.Deletions, nil
	case "changeType":
		return change.Status, nil
	}
	return nil, nil
}

func (e *executor) pageInfoField(field *gqlField, value *rtVal) (any, error) {
	switch field.name {
	case "hasNextPage":
		return value.hasNext, nil
	case "hasPreviousPage":
		return false, nil
	case "endCursor":
		if value.endCursor == "" {
			return nil, nil
		}
		return value.endCursor, nil
	case "startCursor":
		return nil, nil
	}
	return nil, nil
}

// -------------------------------------------------------------------------------------------
// mutations
// -------------------------------------------------------------------------------------------

// executePayload answers a mutation payload: one field (issue or
// pullRequest) whose sub-selection runs against that record.
func (e *executor) executePayload(field *gqlField, payloadField string, value *rtVal) (any, error) {
	out := newObj()
	typeName := "Issue"
	if payloadField == "pullRequest" {
		typeName = "PullRequest"
	}
	for _, sel := range field.sels {
		if sel.kind != selField {
			continue
		}
		sub := sel.field
		if sub.name != payloadField {
			continue
		}
		name := sub.name
		if sub.hasAlias {
			name = sub.alias
		}
		result, err := e.executeSelectionSet(sub.sels, typeName, value)
		if err != nil {
			return nil, err
		}
		out.set(name, result)
	}
	return out, nil
}

func (e *executor) mutationField(field *gqlField) (any, error) {
	input, ok := argValue(field, "input", e.vars).(map[string]any)
	if !ok {
		return nil, errf("mutation %s needs an input object", field.name)
	}
	switch field.name {
	case "createIssue":
		repositoryID, ok := asString(input["repositoryId"])
		if !ok {
			return nil, errf("createIssue needs repositoryId")
		}
		decoded := e.st.DecodeGlobalID(repositoryID)
		if decoded == nil || decoded.Kind != "repo" {
			return nil, errf("createIssue: repository not found")
		}
		title, ok := asString(input["title"])
		if !ok || title == "" {
			return nil, errf("createIssue needs a title")
		}
		body, _ := asString(input["body"])
		created, errMsg := e.st.CreateIssue(decoded.Repo, title, body, []string{})
		if errMsg != "" {
			return nil, errf("%s", errMsg)
		}
		return e.executePayload(field, "issue", &rtVal{kind: rtIssue, repo: decoded.Repo, issue: created})
	case "closeIssue":
		issueID, ok := asString(input["issueId"])
		if !ok {
			return nil, errf("closeIssue needs issueId")
		}
		decoded := e.st.DecodeGlobalID(issueID)
		if decoded == nil || decoded.Kind != "issue" {
			return nil, errf("closeIssue: issue not found")
		}
		decoded.Issue.State = "closed"
		decoded.Issue.ViewBuf = nil
		return e.executePayload(field, "issue", &rtVal{kind: rtIssue, repo: decoded.Repo, issue: decoded.Issue})
	case "createPullRequest":
		repositoryID, ok := asString(input["repositoryId"])
		if !ok {
			return nil, errf("createPullRequest needs repositoryId")
		}
		decoded := e.st.DecodeGlobalID(repositoryID)
		if decoded == nil || decoded.Kind != "repo" {
			return nil, errf("createPullRequest: repository not found")
		}
		headRefName, ok := asString(input["headRefName"])
		if !ok {
			return nil, errf("createPullRequest needs headRefName, baseRefName and title")
		}
		baseRefName, ok := asString(input["baseRefName"])
		if !ok {
			return nil, errf("createPullRequest needs headRefName, baseRefName and title")
		}
		title, ok := asString(input["title"])
		if !ok {
			return nil, errf("createPullRequest needs headRefName, baseRefName and title")
		}
		body, _ := asString(input["body"])
		created, errMsg := e.st.CreatePull(decoded.Repo, title, body, headRefName, baseRefName)
		if errMsg != "" {
			return nil, errf("%s", errMsg)
		}
		return e.executePayload(field, "pullRequest", &rtVal{kind: rtPull, repo: decoded.Repo, pull: created})
	case "mergePullRequest":
		pullRequestID, ok := asString(input["pullRequestId"])
		if !ok {
			return nil, errf("mergePullRequest needs pullRequestId")
		}
		decoded := e.st.DecodeGlobalID(pullRequestID)
		if decoded == nil || decoded.Kind != "pull" {
			return nil, errf("mergePullRequest: pull request not found")
		}
		if _, errMsg, _ := e.st.MergePull(decoded.Repo, decoded.Pull); errMsg != "" {
			return nil, errf("%s", errMsg)
		}
		return e.executePayload(field, "pullRequest", &rtVal{kind: rtPull, repo: decoded.Repo, pull: decoded.Pull})
	case "addComment":
		subjectID, ok := asString(input["subjectId"])
		if !ok {
			return nil, errf("addComment needs subjectId")
		}
		decoded := e.st.DecodeGlobalID(subjectID)
		if decoded == nil || decoded.Kind != "issue" {
			return nil, errf("addComment: issue not found")
		}
		body, ok := asString(input["body"])
		if !ok {
			return nil, errf("addComment needs body")
		}
		comment := e.st.CreateComment(decoded.Repo, decoded.Issue, body)
		out := newObj()
		for _, sel := range field.sels {
			if sel.kind != selField {
				continue
			}
			sub := sel.field
			if sub.name != "commentEdge" {
				continue
			}
			edge := newObj()
			for _, edgeSel := range sub.sels {
				if edgeSel.kind != selField {
					continue
				}
				nodeField := edgeSel.field
				if nodeField.name != "node" {
					continue
				}
				node := newObj()
				for _, nodeSel := range nodeField.sels {
					if nodeSel.kind != selField {
						continue
					}
					leaf := nodeSel.field
					key := leaf.name
					if leaf.hasAlias {
						key = leaf.alias
					}
					switch leaf.name {
					case "id":
						node.set(key, e.st.CommentGlobalID(decoded.Repo, decoded.Issue, comment))
					case "body":
						node.set(key, comment.Body)
					}
				}
				edgeName := nodeField.name
				if nodeField.hasAlias {
					edgeName = nodeField.alias
				}
				edge.set(edgeName, node)
			}
			name := sub.name
			if sub.hasAlias {
				name = sub.alias
			}
			out.set(name, edge)
		}
		return out, nil
	}
	return nil, nil
}

// -------------------------------------------------------------------------------------------
// one operation, and the HTTP endpoint
// -------------------------------------------------------------------------------------------

// executeOne runs one operation document (or one entry of a batched body)
// and answers its {data, errors} envelope.
func executeOne(st *store.Store, body any) *jobj {
	envelope := func(data any, errors []any) *jobj {
		return newObj().set("data", data).set("errors", errors)
	}
	fail := func(message string) *jobj {
		return envelope(nil, []any{newObj().set("message", message)})
	}
	op, ok := body.(map[string]any)
	if !ok {
		return fail("a GraphQL operation needs a query string")
	}
	query, ok := op["query"].(string)
	if !ok {
		return fail("a GraphQL operation needs a query string")
	}
	vars := map[string]any{}
	if v, ok := op["variables"].(map[string]any); ok {
		vars = v
	}
	tokens, err := tokenize(query)
	if err != nil {
		return fail("could not parse the query: " + err.Error())
	}
	p := &parser{tokens: tokens}
	operations, err := p.parseDocument()
	if err != nil {
		return fail("could not parse the query: " + err.Error())
	}
	if len(operations) == 0 {
		return fail("the query holds no operation")
	}
	chosen := operations[0]
	if name, ok := op["operationName"].(string); ok {
		chosen = nil
		for _, candidate := range operations {
			if candidate.hasName && candidate.name == name {
				chosen = candidate
				break
			}
		}
		if chosen == nil {
			return fail("no operation named " + name)
		}
	}
	ex := &executor{st: st, vars: vars}
	data, err := ex.executeOperation(chosen)
	if err != nil {
		if gqlErr, ok := err.(*gqlError); ok {
			return fail(gqlErr.msg)
		}
		return fail("internal error: " + err.Error())
	}
	return envelope(data, []any{})
}

// handle runs the endpoint's logic: authentication first, then one operation
// or a batch of them, in order.
func handle(st *store.Store, body []byte, authed bool) (int, []byte) {
	if !authed {
		raw, _ := marshalJSON(newObj().set("message", "Requires authentication"))
		return 401, append(raw, '\n')
	}
	var parsed any
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&parsed); err != nil {
		raw, _ := marshalJSON(newObj().set("message", "Invalid JSON"))
		return 400, append(raw, '\n')
	}
	if batch, ok := parsed.([]any); ok {
		results := make([]any, 0, len(batch))
		for _, entry := range batch {
			results = append(results, executeOne(st, entry))
		}
		raw, err := marshalJSON(results)
		if err != nil {
			raw, _ = marshalJSON(newObj().set("message", "Internal Server Error"))
			return 500, append(raw, '\n')
		}
		return 200, append(raw, '\n')
	}
	raw, err := marshalJSON(executeOne(st, parsed))
	if err != nil {
		raw, _ = marshalJSON(newObj().set("message", "Internal Server Error"))
		return 500, append(raw, '\n')
	}
	return 200, append(raw, '\n')
}

// Register mounts the GraphQL v4 endpoint on the router. The router
// answers any other method on this path with 405 itself.
func Register(r *httpd.Router, st *store.Store) {
	r.Handle("POST", "/api/graphql", func(c *httpd.Ctx) httpd.Result {
		status, raw := handle(st, c.Body, c.Authed)
		return httpd.JSONRaw(status, raw)
	})
}
