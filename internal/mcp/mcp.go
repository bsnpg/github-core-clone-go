// Package mcp is the Model Context Protocol surface at POST /mcp:
// JSON-RPC 2.0 over Streamable HTTP, answered as one JSON body per
// request; no SSE stream.
//
// The modern 2026-07-28 era: server/discover answers identity,
// capabilities and supportedVersions in one request; there is no
// handshake and no session. The legacy handshake (initialize +
// notifications/initialized) is also answered, so a pre-discover client
// still connects. Every request needs the bearer token (a missing token
// is a 401, decided before any tool exists, and before the body is even
// parsed).
//
// Errors: an unknown tool is JSON-RPC -32602 (never HTTP 500); a tool
// called without its required arguments answers isError: true; a body
// that is not JSON is HTTP 400 with JSON-RPC -32700. Two concurrent calls
// keep their own JSON-RPC ids (the request id is echoed verbatim, numbers
// kept as json.Number so they round-trip exactly), and two tools/list
// answers are byte-identical (the tool list is a constant, marshalled
// from the same structs every time).
//
// The tools act on the same store as REST: a repository and an issue
// created over MCP read back over /api/v3 as the same objects. The
// repository and issue views here are the same shapes rest.go builds.
package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"forge/internal/httpd"
	"forge/internal/store"
)

// Revision is the protocol revision this server speaks.
const Revision = "2026-07-28"

// SupportedVersions is every revision the server answers for.
var SupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18"}

// ServerName and ServerVersion identify this server.
const (
	ServerName    = "github-core-clone"
	ServerVersion = "1.0.0"
)

// Register wires POST /mcp onto r. The dispatcher has already set
// Ctx.Authed from the Authorization header.
func Register(r *httpd.Router, st *store.Store) {
	r.Handle("POST", "/mcp", func(c *httpd.Ctx) httpd.Result {
		return handleMcp(st, c.Body, c.Authed)
	})
}

// -------------------------------------------------------------------------------------------
// the tool list (a constant: two tools/list calls answer it byte for byte)
// -------------------------------------------------------------------------------------------

// propSchema is one input-schema property.
type propSchema struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Enum        []string `json:"enum,omitempty"`
}

// inputSchema is a tool's JSON Schema for its arguments.
type inputSchema struct {
	Type                 string                `json:"type"`
	Properties           map[string]propSchema `json:"properties"`
	Required             []string              `json:"required"`
	AdditionalProperties bool                  `json:"additionalProperties"`
}

// toolDef is one tool on tools/list.
type toolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
}

func strProp(desc string) propSchema { return propSchema{Type: "string", Description: desc} }

// tools is the whole list, in the order the Node entry offers it.
var tools = []toolDef{
	{
		Name:        "create_repo",
		Description: "Create a repository owned by the authenticated user.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]propSchema{
				"name":        strProp("The repository name."),
				"description": strProp("The repository description."),
				"private":     {Type: "boolean", Description: "Whether the repository is private."},
			},
			Required:             []string{"name"},
			AdditionalProperties: false,
		},
	},
	{
		Name:        "get_repo",
		Description: "Read one repository by owner and name.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]propSchema{
				"owner": strProp("The repository owner."),
				"repo":  strProp("The repository name."),
			},
			Required:             []string{"owner", "repo"},
			AdditionalProperties: false,
		},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]propSchema{
				"owner": strProp("The repository owner."),
				"repo":  strProp("The repository name."),
				"title": strProp("The issue title."),
				"body":  strProp("The issue body."),
			},
			Required:             []string{"owner", "repo", "title"},
			AdditionalProperties: false,
		},
	},
	{
		Name:        "list_issues",
		Description: "List the issues of a repository, optionally filtered by state.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]propSchema{
				"owner": strProp("The repository owner."),
				"repo":  strProp("The repository name."),
				"state": {Type: "string", Description: "open, closed or all.", Enum: []string{"open", "closed", "all"}},
			},
			Required:             []string{"owner", "repo"},
			AdditionalProperties: false,
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Open a pull request from a head branch into a base branch.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]propSchema{
				"owner": strProp("The repository owner."),
				"repo":  strProp("The repository name."),
				"title": strProp("The pull request title."),
				"body":  strProp("The pull request body."),
				"head":  strProp("The head branch."),
				"base":  strProp("The base branch."),
			},
			Required:             []string{"owner", "repo", "title", "head", "base"},
			AdditionalProperties: false,
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Squash-merge a pull request.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]propSchema{
				"owner":       strProp("The repository owner."),
				"repo":        strProp("The repository name."),
				"pull_number": {Type: "integer", Description: "The pull request number."},
			},
			Required:             []string{"owner", "repo", "pull_number"},
			AdditionalProperties: false,
		},
	},
}

// -------------------------------------------------------------------------------------------
// JSON-RPC envelope
// -------------------------------------------------------------------------------------------

// rpcError is the error member of a JSON-RPC response.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcResponse is one JSON-RPC 2.0 response. Field order is fixed
// (jsonrpc, id, result, error) so the bytes are stable call to call.
type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

func rpcResult(id any, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func rpcErr(id any, code int, message string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

// marshalJSON encodes v without HTML escaping and without a trailing
// newline, the way the Node entry's JSON.stringify spells it.
func marshalJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(buf.String(), "\n")
}

// toolResult is a successful tool call: the structured value, rendered
// as text as well.
func toolResult(structured any) map[string]any {
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": marshalJSON(structured)}},
		"structuredContent": structured,
		"isError":           false,
	}
}

// toolError is a failed tool call: isError true, no -32602. The caller
// stays HTTP 200.
func toolError(message string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": message}},
		"isError": true,
	}
}

// -------------------------------------------------------------------------------------------
// views (the same shapes rest.go builds)
// -------------------------------------------------------------------------------------------

func repoView(repo *store.RepoRecord) map[string]any {
	return map[string]any{
		"id":             repo.ID,
		"name":           repo.Name,
		"full_name":      repo.Owner + "/" + repo.Name,
		"owner":          map[string]any{"login": repo.Owner, "id": store.Viewer.ID, "type": "User"},
		"private":        repo.Private,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
		"created_at":     repo.CreatedAt,
		"updated_at":     repo.UpdatedAt,
		"html_url":       "https://example.invalid/" + repo.Owner + "/" + repo.Name,
		"url":            "/api/v3/repos/" + repo.Owner + "/" + repo.Name,
	}
}

func issueView(issue *store.IssueRecord, repo *store.RepoRecord) map[string]any {
	labels := make([]any, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		if label := repo.Labels[name]; label != nil {
			labels = append(labels, map[string]any{"name": label.Name, "color": label.Color, "description": label.Description})
		} else {
			labels = append(labels, map[string]any{"name": name, "color": "ededed", "description": ""})
		}
	}
	return map[string]any{
		"id":         issue.ID,
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"user":       map[string]any{"login": issue.User, "id": store.Viewer.ID, "type": "User"},
		"created_at": issue.CreatedAt,
		"updated_at": issue.UpdatedAt,
		"comments":   len(issue.Comments),
		"html_url":   "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
		"url":        "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
	}
}

// -------------------------------------------------------------------------------------------
// arguments
// -------------------------------------------------------------------------------------------

// strArg reads a string argument, trying each name in order.
func strArg(args map[string]any, names ...string) string {
	for _, name := range names {
		if v, ok := args[name].(string); ok {
			return v
		}
	}
	return ""
}

// intArg reads an integer argument (a JSON number, or a numeric string).
func intArg(args map[string]any, names ...string) (int, bool) {
	for _, name := range names {
		switch v := args[name].(type) {
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return int(n), true
			}
		case float64:
			return int(v), true
		case string:
			if v == "" {
				continue
			}
			if n, err := strconv.Atoi(v); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// resolveOwnerRepo reads the owner and repo an argument map names. The
// suite maps each argument onto the schema's nearest property name and
// also fills full_name with owner/repo, so full_name is honoured when
// owner or repo is missing, as is the repository alias for repo.
func resolveOwnerRepo(args map[string]any) (owner, repo string, ok bool) {
	owner = strArg(args, "owner")
	repo = strArg(args, "repo", "repository")
	if owner == "" || repo == "" {
		if full := strArg(args, "full_name"); full != "" {
			if i := strings.Index(full, "/"); i > 0 {
				if owner == "" {
					owner = full[:i]
				}
				if repo == "" {
					repo = full[i+1:]
				}
			}
		}
	}
	return owner, repo, owner != "" && repo != ""
}

// namedRepo is the repository a tool call names, or nil (the owner must
// be the viewer).
func namedRepo(st *store.Store, args map[string]any) *store.RepoRecord {
	owner, repo, ok := resolveOwnerRepo(args)
	if !ok {
		return nil
	}
	if !strings.EqualFold(owner, store.Viewer.Login) {
		return nil
	}
	return st.GetRepo(owner, repo)
}

// missingArg names the first required argument that is absent, null or
// an empty string, or "" when all are present.
func missingArg(args map[string]any, required ...string) string {
	for _, name := range required {
		v, ok := args[name]
		if !ok || v == nil {
			return name
		}
		if s, isStr := v.(string); isStr && s == "" {
			return name
		}
	}
	return ""
}

// normaliseToolName matches a called tool name the way the suite matches
// a listed one: a server prefix is allowed, matched as a suffix, case
// insensitively (github__create_issue calls create_repo's sibling
// create_issue).
func normaliseToolName(name string) string {
	name = strings.ToLower(name)
	if i := strings.LastIndex(name, "__"); i != -1 {
		name = name[i+2:]
	}
	return name
}

func isKnownTool(name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// -------------------------------------------------------------------------------------------
// tools/call
// -------------------------------------------------------------------------------------------

func callTool(st *store.Store, name string, args map[string]any) map[string]any {
	switch name {
	case "create_repo":
		if missing := missingArg(args, "name"); missing != "" {
			return toolError("create_repo needs " + missing)
		}
		repoName := strArg(args, "name")
		if repoName == "" {
			if _, repo, ok := resolveOwnerRepo(args); ok {
				repoName = repo
			}
		}
		description := strArg(args, "description")
		private, _ := args["private"].(bool)
		repo := st.CreateRepo(store.Viewer.Login, repoName, description, private)
		if repo == nil {
			return toolError("repository " + repoName + " already exists")
		}
		return toolResult(repoView(repo))

	case "get_repo":
		if _, _, ok := resolveOwnerRepo(args); !ok {
			return toolError("get_repo needs " + missingArg(args, "owner", "repo"))
		}
		repo := namedRepo(st, args)
		if repo == nil {
			return toolError("repository not found")
		}
		return toolResult(repoView(repo))

	case "create_issue":
		if _, _, ok := resolveOwnerRepo(args); !ok {
			return toolError("create_issue needs " + missingArg(args, "owner", "repo"))
		}
		if missing := missingArg(args, "title"); missing != "" {
			return toolError("create_issue needs " + missing)
		}
		repo := namedRepo(st, args)
		if repo == nil {
			return toolError("repository not found")
		}
		created, errMsg := st.CreateIssue(repo, strArg(args, "title"), strArg(args, "body"), nil)
		if errMsg != "" {
			return toolError(errMsg)
		}
		return toolResult(issueView(created, repo))

	case "list_issues":
		if _, _, ok := resolveOwnerRepo(args); !ok {
			return toolError("list_issues needs " + missingArg(args, "owner", "repo"))
		}
		repo := namedRepo(st, args)
		if repo == nil {
			return toolError("repository not found")
		}
		state := strArg(args, "state")
		if state == "" {
			state = "open"
		}
		views := make([]any, 0, len(repo.Issues))
		for _, issue := range repo.Issues {
			if state != "all" && issue.State != state {
				continue
			}
			views = append(views, issueView(issue, repo))
		}
		return toolResult(views)

	case "create_pull_request":
		if _, _, ok := resolveOwnerRepo(args); !ok {
			return toolError("create_pull_request needs " + missingArg(args, "owner", "repo"))
		}
		if missing := missingArg(args, "title", "head", "base"); missing != "" {
			return toolError("create_pull_request needs " + missing)
		}
		repo := namedRepo(st, args)
		if repo == nil {
			return toolError("repository not found")
		}
		created, errMsg := st.CreatePull(repo, strArg(args, "title"), strArg(args, "body"), strArg(args, "head"), strArg(args, "base"))
		if errMsg != "" {
			return toolError(errMsg)
		}
		return toolResult(map[string]any{
			"id":     created.ID,
			"number": created.Number,
			"title":  created.Title,
			"body":   created.Body,
			"state":  created.State,
			"head":   map[string]any{"ref": created.HeadRef, "sha": created.HeadSha},
			"base":   map[string]any{"ref": created.BaseRef, "sha": created.BaseSha},
		})

	case "merge_pull_request":
		if _, _, ok := resolveOwnerRepo(args); !ok {
			return toolError("merge_pull_request needs " + missingArg(args, "owner", "repo"))
		}
		repo := namedRepo(st, args)
		if repo == nil {
			return toolError("repository not found")
		}
		number, ok := intArg(args, "pull_number", "pullNumber", "number")
		if !ok {
			return toolError("merge_pull_request needs an integer pull_number")
		}
		pull := st.GetPull(repo, number)
		if pull == nil {
			return toolError("pull request not found")
		}
		sha, errMsg, _ := st.MergePull(repo, pull)
		if errMsg != "" {
			return toolError(errMsg)
		}
		return toolResult(map[string]any{"number": pull.Number, "merged": true, "sha": sha, "state": pull.State})

	default:
		return toolError("unknown tool " + name)
	}
}

// -------------------------------------------------------------------------------------------
// the endpoint
// -------------------------------------------------------------------------------------------

// handleMcp answers one POST /mcp body. rawBody is the request's bytes:
// a body that is not JSON is answered HTTP 400 with JSON-RPC -32700.
// authed says whether the bearer token was present and valid.
func handleMcp(st *store.Store, rawBody []byte, authed bool) httpd.Result {
	if !authed {
		return httpd.JSON(401, rpcErr(nil, -32001, "Requires authentication"))
	}
	dec := json.NewDecoder(bytes.NewReader(rawBody))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return httpd.JSON(400, rpcErr(nil, -32700, "Parse error"))
	}
	// Trailing data after the first JSON value is a parse error too,
	// the way JSON.parse treats it.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return httpd.JSON(400, rpcErr(nil, -32700, "Parse error"))
	}
	req, ok := parsed.(map[string]any)
	if !ok {
		return httpd.JSON(400, rpcErr(nil, -32600, "Invalid Request"))
	}
	id, hasID := req["id"]
	if !hasID {
		// A notification (no id) is acknowledged with an empty 202,
		// the way the Node entry answers it.
		return httpd.JSON(202, "")
	}
	method, _ := req["method"].(string)
	params := map[string]any{}
	if p, ok := req["params"].(map[string]any); ok {
		params = p
	}
	if method == "" {
		return httpd.JSON(200, rpcErr(id, -32600, "Invalid Request"))
	}

	switch method {
	case "server/discover":
		return httpd.JSON(200, rpcResult(id, map[string]any{
			"resultType":        "DiscoverResult",
			"supportedVersions": append([]string(nil), SupportedVersions...),
			"capabilities":      map[string]any{"tools": map[string]any{"listChanged": false}},
			"instructions":      "A GitHub-compatible git forge: repositories, issues, pull requests, git smart HTTP, GraphQL and MCP tools.",
			"serverInfo":        map[string]any{"name": ServerName, "version": ServerVersion},
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": ServerName, "version": ServerVersion}},
		}))

	case "initialize":
		requested, _ := params["protocolVersion"].(string)
		negotiated := Revision
		for _, v := range SupportedVersions {
			if v == requested {
				negotiated = v
				break
			}
		}
		return httpd.JSON(200, rpcResult(id, map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": ServerName, "version": ServerVersion},
		}))

	case "ping":
		return httpd.JSON(200, rpcResult(id, map[string]any{}))

	case "tools/list":
		return httpd.JSON(200, rpcResult(id, map[string]any{"tools": tools}))

	case "tools/call":
		toolName, _ := params["name"].(string)
		if toolName == "" {
			return httpd.JSON(200, rpcErr(id, -32602, "tools/call needs a tool name"))
		}
		normalised := normaliseToolName(toolName)
		if !isKnownTool(normalised) {
			return httpd.JSON(200, rpcErr(id, -32602, "unknown tool "+toolName))
		}
		args := map[string]any{}
		if a, ok := params["arguments"].(map[string]any); ok {
			args = a
		}
		return httpd.JSON(200, rpcResult(id, callTool(st, normalised, args)))

	default:
		return httpd.JSON(200, rpcErr(id, -32601, "Method not found: "+method))
	}
}
