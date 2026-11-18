// Package githttp is the git smart-HTTP surface: info/refs, upload-pack
// (protocol v2 ls-refs/fetch, plus a v0 fetch fallback) and receive-pack
// (v0, report-status).
//
// This is a port of the Node reference implementation's git surface,
// translated into Go in my own words. The framing follows git's own wire
// rules: pkt-lines, section delimiters, side-band-64k for packs and
// reports, and fast-forward enforcement on pushes.
//
// The pack writer emits whole objects only, no deltas — the same strategy
// as the reference: a whole-object pack is always valid, and the suite's
// client applies it directly. The pack reader, used by receive-pack,
// resolves OFS_DELTA and REF_DELTA entries (including thin packs whose
// REF_DELTA bases live in the store already).
//
// Locking: the store's methods are not internally locked, so this package
// guards every access with one RWMutex: reads take the read lock, the
// receive-pack mutation path takes the write lock. The pack cache has its
// own mutex so concurrent clones never serialize on it; cached pack bytes
// are immutable and shared read-only.
//
// Two deliberate divergences from the reference's literal text, both
// protocol-correct and noted here:
//   - A v0 "want" line may carry the client's capabilities after a NUL
//     byte ("want <oid>\0<capabilities>"). The reference's parser takes
//     the first space-separated token as the oid, so a NUL-carrying first
//     want fails its oid check and is silently dropped. This port strips
//     at the NUL first, so the want is honoured. With the space-separated
//     form both parsers agree.
//   - GET info/refs with a missing or unknown ?service= answers 400
//     {"message": "service is required"}: that is the reference's HTTP
//     layer behaviour (its git handler itself would answer 404, but the
//     dispatcher rejects the request before calling it).
//   - No authentication check is done here, matching the reference: its
//     dispatcher does not auth-check the git routes either.
package githttp

import (
	"bytes"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"forge/internal/gitobj"
	"forge/internal/httpd"
	"forge/internal/store"
	"forge/internal/util"
)

const (
	agentName = "forge/1.0.0"

	uploadAdvertisement  = "application/x-git-upload-pack-advertisement"
	uploadResult         = "application/x-git-upload-pack-result"
	receiveAdvertisement = "application/x-git-receive-pack-advertisement"
	receiveResult        = "application/x-git-receive-pack-result"
)

// packCacheMax bounds the pack cache so a pathological trial cannot grow
// it without bound; the oldest entries go first.
const packCacheMax = 128

// gitServer is the state Register closes over: the store, one lock for
// every store access, and the pack cache.
type gitServer struct {
	store *store.Store

	// mu guards every read and write of the store through this package.
	mu sync.RWMutex

	// cacheMu guards the pack cache alone.
	cacheMu    sync.Mutex
	packCache  map[string][]byte
	cacheOrder []string
}

// Register wires the git smart-HTTP routes onto r. The router matches
// exact paths before "*" prefixes, so registering the catch-all "/*"
// prefix here only catches paths no more specific route claims; the
// dispatchers below answer 404 JSON for any non-git path that reaches
// them, matching the reference dispatcher's fallback.
func Register(r *httpd.Router, st *store.Store) {
	s := &gitServer{store: st, packCache: map[string][]byte{}}
	r.Handle("GET", "/*", s.dispatchGet)
	r.Handle("POST", "/*", s.dispatchPost)
}

// jsonError answers a JSON {"message": ...} error.
func jsonError(status int, message string) httpd.Result {
	return httpd.JSON(status, map[string]string{"message": message})
}

// decodeSegment percent-decodes a path segment only when it actually
// carries an escape; a bad escape falls back to the raw segment.
func decodeSegment(value string) string {
	if !strings.Contains(value, "%") {
		return value
	}
	if decoded, err := url.PathUnescape(value); err == nil {
		return decoded
	}
	return value
}

// parseGitPath splits a request path into owner, repo and the tail after
// "/{owner}/{repo}.git". It reports false when the path is not a git path
// at all.
func parseGitPath(path string) (owner, repo string, tail []string, ok bool) {
	var segments []string
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			segments = append(segments, part)
		}
	}
	if len(segments) < 2 || !strings.HasSuffix(segments[1], ".git") {
		return "", "", nil, false
	}
	owner = decodeSegment(segments[0])
	repo = decodeSegment(segments[1][:len(segments[1])-len(".git")])
	return owner, repo, segments[2:], true
}

// queryService reads the ?service= parameter of the raw query string.
func queryService(rawQuery string) string {
	rawQuery = strings.TrimPrefix(rawQuery, "?")
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return ""
	}
	return values.Get("service")
}

func (s *gitServer) dispatchGet(c *httpd.Ctx) httpd.Result {
	owner, repo, tail, ok := parseGitPath(c.Path)
	if !ok || len(tail) != 2 || tail[0] != "info" || tail[1] != "refs" {
		return jsonError(404, "Not Found")
	}
	return s.handleInfoRefs(c, owner, repo)
}

func (s *gitServer) dispatchPost(c *httpd.Ctx) httpd.Result {
	owner, repo, tail, ok := parseGitPath(c.Path)
	if !ok || len(tail) != 1 {
		return jsonError(404, "Not Found")
	}
	switch tail[0] {
	case "git-upload-pack":
		return s.handleUploadPack(c, owner, repo)
	case "git-receive-pack":
		return s.handleReceivePack(c, owner, repo)
	default:
		return jsonError(404, "Not Found")
	}
}

// -------------------------------------------------------------------------------------------
// advertisements
// -------------------------------------------------------------------------------------------

// sortedRefs lists a repo's refs sorted by name.
func sortedRefs(repo *store.RepoRecord) []refEntry {
	refs := make([]refEntry, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		refs = append(refs, refEntry{name: name, oid: oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	return refs
}

type refEntry struct {
	name string
	oid  string
}

// peeledOid is the commit an annotated tag ref peels to. The second value
// reports whether the object is a tag at all.
func peeledOid(repo *store.RepoRecord, oid string) (string, bool) {
	object := repo.Objects[oid]
	if object == nil || object.Type != gitobj.Tag {
		return "", false
	}
	target, _ := gitobj.ParseTagTarget(object.Data)
	return target, true
}

// v2Advertisement is the protocol v2 capability advertisement. It opens
// with "version 2" and carries no "# service=" preamble.
func v2Advertisement() []byte {
	var buf bytes.Buffer
	for _, line := range []string{
		"version 2\n",
		"agent=" + agentName + "\n",
		"ls-refs=unborn\n",
		"fetch=shallow filter\n",
		"object-format=sha1\n",
	} {
		buf.Write(encodePktLineString(line))
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

// v0Advertisement is the v0 advertisement for a service, with the ref list
// and capabilities, opening with the "# service=" preamble and a flush.
func v0Advertisement(repo *store.RepoRecord, service string) []byte {
	var caps string
	if service == "git-receive-pack" {
		caps = "report-status side-band-64k delete-refs agent=" + agentName +
			" symref=HEAD:refs/heads/" + repo.DefaultBranch
	} else {
		caps = "side-band-64k agent=" + agentName +
			" symref=HEAD:refs/heads/" + repo.DefaultBranch + " object-format=sha1"
	}
	var buf bytes.Buffer
	buf.Write(encodePktLineString("# service=" + service + "\n"))
	buf.Write(pktFlush)
	refs := sortedRefs(repo)
	if len(refs) == 0 {
		buf.Write(encodePktLineString(util.ZeroOID + " capabilities^{}\x00" + caps + "\n"))
	} else {
		mainTip, hasMain := repo.Refs["refs/heads/"+repo.DefaultBranch]
		headLine := ""
		if hasMain {
			headLine = mainTip + " HEAD\x00" + caps + "\n"
			buf.Write(encodePktLineString(headLine))
		}
		// The ref lines, kept aside so the capabilities can ride the
		// first one when there is no default-branch tip.
		lines := make([]string, 0, len(refs)*2)
		for _, ref := range refs {
			lines = append(lines, ref.oid+" "+ref.name+"\n")
			if peeled, isTag := peeledOid(repo, ref.oid); isTag {
				lines = append(lines, peeled+" "+ref.name+"^{}\n")
			}
		}
		if headLine == "" && len(lines) > 0 {
			lines[0] = strings.Replace(lines[0], "\n", "\x00"+caps+"\n", 1)
		}
		for _, line := range lines {
			buf.Write(encodePktLineString(line))
		}
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

// handleInfoRefs answers GET /{owner}/{repo}.git/info/refs?service=....
func (s *gitServer) handleInfoRefs(c *httpd.Ctx, owner, repoName string) httpd.Result {
	service := queryService(c.Query)
	if service != "git-upload-pack" && service != "git-receive-pack" {
		return jsonError(400, "service is required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		return jsonError(404, "Not Found")
	}
	if service == "git-upload-pack" && c.Headers["git-protocol"] == "version=2" {
		return httpd.Bytes(200, v2Advertisement(), uploadAdvertisement)
	}
	body := v0Advertisement(repo, service)
	contentType := uploadAdvertisement
	if service == "git-receive-pack" {
		contentType = receiveAdvertisement
	}
	return httpd.Bytes(200, body, contentType)
}

// -------------------------------------------------------------------------------------------
// ls-refs (v2)
// -------------------------------------------------------------------------------------------

type lsRefsArgs struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

func (s *gitServer) handleLsRefs(repo *store.RepoRecord, packets []packet) []byte {
	var args lsRefsArgs
	for _, p := range packets {
		if p.kind != packetData {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case line == "peel":
			args.peel = true
		case line == "symrefs":
			args.symrefs = true
		case line == "unborn":
			args.unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			args.prefixes = append(args.prefixes, line[len("ref-prefix "):])
		}
	}
	keep := func(name string) bool {
		if len(args.prefixes) == 0 {
			return true
		}
		for _, prefix := range args.prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	var buf bytes.Buffer
	mainTip, hasMain := repo.Refs["refs/heads/"+repo.DefaultBranch]
	if hasMain {
		if keep("HEAD") {
			line := mainTip + " HEAD"
			if args.symrefs {
				line += " symref-target:refs/heads/" + repo.DefaultBranch
			}
			buf.Write(encodePktLineString(line + "\n"))
		}
	} else if args.unborn && keep("HEAD") {
		line := "unborn HEAD"
		if args.symrefs {
			line += " symref-target:refs/heads/" + repo.DefaultBranch
		}
		buf.Write(encodePktLineString(line + "\n"))
	}
	for _, ref := range sortedRefs(repo) {
		if !keep(ref.name) {
			continue
		}
		line := ref.oid + " " + ref.name
		if args.peel {
			if peeled, isTag := peeledOid(repo, ref.oid); isTag {
				line += " peeled:" + peeled
			}
		}
		buf.Write(encodePktLineString(line + "\n"))
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

// -------------------------------------------------------------------------------------------
// fetch (v2 + v0)
// -------------------------------------------------------------------------------------------

type fetchArgs struct {
	wants            []string
	haves            []string
	shallow          []string
	depth            *int
	done             bool
	shallowRequested bool
}

func parseFetchArgs(packets []packet) fetchArgs {
	var args fetchArgs
	for _, p := range packets {
		if p.kind != packetData {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			rest := line[len("want "):]
			// A v0 first want line may carry the client's capabilities
			// after a NUL byte; the oid is what precedes it.
			if i := strings.IndexByte(rest, 0); i >= 0 {
				rest = rest[:i]
			}
			oid := strings.SplitN(rest, " ", 2)[0]
			if util.IsOID(oid) {
				args.wants = append(args.wants, oid)
			}
		case strings.HasPrefix(line, "have "):
			args.haves = append(args.haves, strings.TrimSpace(line[len("have "):]))
		case strings.HasPrefix(line, "shallow "):
			args.shallow = append(args.shallow, strings.TrimSpace(line[len("shallow "):]))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.ParseFloat(strings.TrimSpace(line[len("deepen "):]), 64); err == nil {
				d := int(n)
				args.depth = &d
			}
			args.shallowRequested = true
		case strings.HasPrefix(line, "deepen-since "), strings.HasPrefix(line, "deepen-not "):
			args.shallowRequested = true
		case line == "done":
			args.done = true
		}
	}
	return args
}

type fetchPlan struct {
	packObjects []*gitobj.GitObject
	// packOids is the pack objects' oids, in the same order as
	// packObjects (the pack-cache key).
	packOids   []string
	shallow    []string
	unshallow  []string
	wantedRefs []refEntry
	acks       []string
}

// cachedPack returns the pack bytes for one exact object set, shared
// across identical fetches. A pack holds only objects already in the
// store, and objects are never mutated or deleted, so a cached pack stays
// valid for the trial's life; the key is the repo id plus the sorted
// object ids. The bytes are immutable and shared read-only.
func (s *gitServer) cachedPack(repoID int, packOids []string, packObjects []*gitobj.GitObject) []byte {
	key := strconv.Itoa(repoID) + ":" + strings.Join(packOids, ",")
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if hit, ok := s.packCache[key]; ok {
		return hit
	}
	pack := writePack(packObjects)
	if len(s.packCache) >= packCacheMax {
		oldest := s.cacheOrder[0]
		s.cacheOrder = s.cacheOrder[1:]
		delete(s.packCache, oldest)
	}
	s.packCache[key] = pack
	s.cacheOrder = append(s.cacheOrder, key)
	return pack
}

// planFetch works out what a fetch sends. depth is the deepen value
// (1 = the tips only); without it the whole history goes. Objects the
// client already holds are computed from its haves, stopping at its
// shallow boundaries.
func planFetch(repo *store.RepoRecord, args fetchArgs) fetchPlan {
	lookup := func(oid string) *gitobj.GitObject { return repo.Objects[oid] }
	// One tree's reachable objects, memoised per plan: a full fetch walks
	// one tree per commit, and the subtrees are shared between those trees.
	treeCache := map[string]map[string]*gitobj.GitObject{}
	treeObjects := func(treeOID string) map[string]*gitobj.GitObject {
		if hit, ok := treeCache[treeOID]; ok {
			return hit
		}
		found := gitobj.CollectReachable([]string{treeOID}, lookup)
		treeCache[treeOID] = found
		return found
	}
	// Commit distances from the wants (want = 1, its parent = 2, ...).
	dist := map[string]int{}
	var queue []string
	var commitOrder []string
	includedCommits := map[string]bool{}
	includedObjects := map[string]*gitobj.GitObject{}
	addCommitTree := func(commitOID string) {
		commit := repo.Objects[commitOID]
		if commit == nil || commit.Type != gitobj.Commit {
			return
		}
		treeOID := gitobj.ParseCommit(commit.Data).Tree
		for oid, object := range treeObjects(treeOID) {
			includedObjects[oid] = object
		}
	}
	for _, want := range args.wants {
		object := repo.Objects[want]
		if object == nil {
			continue
		}
		switch object.Type {
		case gitobj.Tag:
			includedObjects[want] = object
			target, _ := gitobj.ParseTagTarget(object.Data)
			targetObject := repo.Objects[target]
			if targetObject != nil && targetObject.Type == gitobj.Commit {
				if _, ok := dist[target]; !ok {
					dist[target] = 1
					queue = append(queue, target)
				}
			} else if targetObject != nil {
				includedObjects[target] = targetObject
			}
		case gitobj.Commit:
			if _, ok := dist[want]; !ok {
				dist[want] = 1
				queue = append(queue, want)
			}
		default:
			includedObjects[want] = object
		}
	}
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		d := dist[oid]
		if args.depth != nil && d > *args.depth {
			continue
		}
		if includedCommits[oid] {
			continue
		}
		includedCommits[oid] = true
		commitOrder = append(commitOrder, oid)
		object := repo.Objects[oid]
		if object == nil {
			continue
		}
		includedObjects[oid] = object
		addCommitTree(oid)
		if args.depth != nil && d >= *args.depth {
			continue
		}
		for _, parent := range gitobj.ParseCommit(object.Data).Parents {
			if _, ok := dist[parent]; !ok {
				dist[parent] = d + 1
				queue = append(queue, parent)
			}
		}
	}
	var shallow, unshallow []string
	shallowSet := map[string]bool{}
	for _, oid := range args.shallow {
		shallowSet[oid] = true
	}
	if args.depth != nil {
		for _, oid := range commitOrder {
			if dist[oid] == *args.depth {
				shallow = append(shallow, oid)
			}
		}
		for _, oid := range args.shallow {
			if includedCommits[oid] && dist[oid] < *args.depth {
				unshallow = append(unshallow, oid)
			}
		}
	} else {
		for _, oid := range args.shallow {
			if includedCommits[oid] {
				unshallow = append(unshallow, oid)
			}
		}
	}
	// What the client already holds: the haves' closure, not crossing its
	// shallow boundaries.
	held := map[string]bool{}
	heldQueue := append([]string(nil), args.haves...)
	heldSeen := map[string]bool{}
	for head := 0; head < len(heldQueue); head++ {
		oid := heldQueue[head]
		if heldSeen[oid] {
			continue
		}
		heldSeen[oid] = true
		object := repo.Objects[oid]
		if object == nil {
			continue
		}
		held[oid] = true
		switch object.Type {
		case gitobj.Commit:
			treeOID := gitobj.ParseCommit(object.Data).Tree
			for treeOID2 := range treeObjects(treeOID) {
				held[treeOID2] = true
			}
			if !shallowSet[oid] {
				heldQueue = append(heldQueue, gitobj.ParseCommit(object.Data).Parents...)
			}
		case gitobj.Tag:
			target, _ := gitobj.ParseTagTarget(object.Data)
			held[target] = true
		}
	}
	// The pack's objects, sorted by id.
	var packOids []string
	for oid := range includedObjects {
		if !held[oid] {
			packOids = append(packOids, oid)
		}
	}
	sort.Strings(packOids)
	packObjects := make([]*gitobj.GitObject, len(packOids))
	for i, oid := range packOids {
		packObjects[i] = includedObjects[oid]
	}
	var wantedRefs []refEntry
	for _, want := range args.wants {
		for _, ref := range sortedRefs(repo) {
			if ref.oid == want {
				wantedRefs = append(wantedRefs, refEntry{oid: want, name: ref.name})
			}
		}
	}
	var acks []string
	for _, have := range args.haves {
		if _, ok := repo.Objects[have]; ok {
			acks = append(acks, have)
		}
	}
	return fetchPlan{
		packObjects: packObjects,
		packOids:    packOids,
		shallow:     shallow,
		unshallow:   unshallow,
		wantedRefs:  wantedRefs,
		acks:        acks,
	}
}

// sidebandPackets wraps bytes in sideband band-1 packets (each at most
// 65520 bytes of framing, so a data chunk is at most 65515 bytes after
// the band byte).
func sidebandPackets(pack []byte) [][]byte {
	const chunk = 65515
	var parts [][]byte
	if len(pack) == 0 {
		return [][]byte{encodePktLine([]byte{1})}
	}
	for offset := 0; offset < len(pack); offset += chunk {
		end := offset + chunk
		if end > len(pack) {
			end = len(pack)
		}
		chunkPayload := make([]byte, 0, 1+end-offset)
		chunkPayload = append(chunkPayload, 1)
		chunkPayload = append(chunkPayload, pack[offset:end]...)
		parts = append(parts, encodePktLine(chunkPayload))
	}
	return parts
}

func (s *gitServer) handleFetchV2(repo *store.RepoRecord, packets []packet) []byte {
	args := parseFetchArgs(packets)
	plan := planFetch(repo, args)
	pack := s.cachedPack(repo.ID, plan.packOids, plan.packObjects)
	var buf bytes.Buffer
	// The response shape depends on whether the client will negotiate
	// further. A `done` request is stateless: the server answers the pack
	// directly — no acknowledgments section and no wanted-refs. Only a
	// shallow fetch adds a shallow-info section (present even when it
	// holds no lines).
	if !args.done {
		buf.Write(encodePktLineString("acknowledgments\n"))
		if len(plan.acks) == 0 {
			buf.Write(encodePktLineString("NAK\n"))
		} else {
			for _, ack := range plan.acks {
				buf.Write(encodePktLineString("ACK " + ack + "\n"))
			}
		}
		buf.Write(pktDelim)
	}
	if args.shallowRequested {
		buf.Write(encodePktLineString("shallow-info\n"))
		for _, oid := range plan.shallow {
			buf.Write(encodePktLineString("shallow " + oid + "\n"))
		}
		for _, oid := range plan.unshallow {
			buf.Write(encodePktLineString("unshallow " + oid + "\n"))
		}
		buf.Write(pktDelim)
	}
	if !args.done && len(plan.wantedRefs) > 0 {
		buf.Write(encodePktLineString("wanted-refs\n"))
		for _, ref := range plan.wantedRefs {
			buf.Write(encodePktLineString(ref.oid + " " + ref.name + "\n"))
		}
		buf.Write(pktDelim)
	}
	buf.Write(encodePktLineString("packfile\n"))
	for _, part := range sidebandPackets(pack) {
		buf.Write(part)
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

func (s *gitServer) handleFetchV0(repo *store.RepoRecord, packets []packet) []byte {
	args := parseFetchArgs(packets)
	plan := planFetch(repo, args)
	pack := s.cachedPack(repo.ID, plan.packOids, plan.packObjects)
	var buf bytes.Buffer
	if len(plan.acks) == 0 {
		buf.Write(encodePktLineString("NAK\n"))
	} else {
		for _, ack := range plan.acks {
			buf.Write(encodePktLineString("ACK " + ack + "\n"))
		}
	}
	for _, oid := range plan.shallow {
		buf.Write(encodePktLineString("shallow " + oid + "\n"))
	}
	for _, oid := range plan.unshallow {
		buf.Write(encodePktLineString("unshallow " + oid + "\n"))
	}
	for _, part := range sidebandPackets(pack) {
		buf.Write(part)
	}
	return buf.Bytes()
}

// handleUploadPack answers POST /{owner}/{repo}.git/git-upload-pack.
func (s *gitServer) handleUploadPack(c *httpd.Ctx, owner, repoName string) (result httpd.Result) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		return jsonError(404, "Not Found")
	}
	packets, err := decodePktLines(c.Body)
	if err != nil {
		return httpd.Bytes(400, []byte("malformed pkt-line stream"), "text/plain")
	}
	firstLine := ""
	for _, p := range packets {
		if p.kind == packetData {
			firstLine = strings.TrimSuffix(string(p.payload), "\n")
			break
		}
	}
	// A failure inside the fetch answers 500 rather than taking the
	// server down with the request.
	defer func() {
		if rec := recover(); rec != nil {
			result = httpd.JSON(500, map[string]string{"message": fmt.Sprintf("fetch failed: %v", rec)})
		}
	}()
	switch firstLine {
	case "command=ls-refs":
		return httpd.Bytes(200, s.handleLsRefs(repo, packets), uploadResult)
	case "command=fetch":
		return httpd.Bytes(200, s.handleFetchV2(repo, packets), uploadResult)
	default:
		// Anything else is a v0 fetch request (want/have/done lines).
		return httpd.Bytes(200, s.handleFetchV0(repo, packets), uploadResult)
	}
}

// -------------------------------------------------------------------------------------------
// receive-pack (v0)
// -------------------------------------------------------------------------------------------

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

// handleReceivePack answers POST /{owner}/{repo}.git/git-receive-pack.
func (s *gitServer) handleReceivePack(c *httpd.Ctx, owner, repoName string) httpd.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		return jsonError(404, "Not Found")
	}
	// Walk the raw body pkt-line by pkt-line until the flush: everything
	// before it is the command list, everything after it is the pack (raw
	// binary that must never be fed to the pkt-line decoder).
	commandEnd := -1
	packStart := len(c.Body)
	{
		offset := 0
		malformed := false
		for offset < len(c.Body) {
			if offset+4 > len(c.Body) {
				malformed = true
				break
			}
			head := string(c.Body[offset : offset+4])
			if head == "0000" {
				commandEnd = offset + 4
				packStart = offset + 4
				break
			}
			length, err := strconv.ParseUint(head, 16, 32)
			if err != nil || length < 4 || offset+int(length) > len(c.Body) {
				malformed = true
				break
			}
			offset += int(length)
		}
		if malformed || commandEnd == -1 {
			return httpd.Bytes(400, []byte("malformed pkt-line stream"), "text/plain")
		}
	}
	packets, err := decodePktLines(c.Body[:commandEnd])
	if err != nil {
		return httpd.Bytes(400, []byte("malformed pkt-line stream"), "text/plain")
	}
	var commands []pushCommand
	sideband := false
	for i, p := range packets {
		if p.kind != packetData {
			continue
		}
		raw := strings.TrimSuffix(string(p.payload), "\n")
		line, caps := raw, ""
		if nul := strings.IndexByte(raw, 0); nul >= 0 {
			line, caps = raw[:nul], raw[nul+1:]
		}
		if i == 0 && strings.Contains(caps, "side-band-64k") {
			sideband = true
		}
		parts := strings.Split(line, " ")
		if len(parts) != 3 {
			continue
		}
		commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
	}
	packBytes := c.Body[packStart:]
	unpackOK := true
	unpackDetail := "ok"
	if len(packBytes) > 0 {
		objects, err := readPack(packBytes, s.store.Lookup(repo))
		if err != nil {
			unpackOK = false
			unpackDetail = err.Error()
		} else {
			s.store.PutObjects(repo, objects)
		}
	}
	var reportLines []string
	if unpackOK {
		reportLines = append(reportLines, "unpack ok\n")
		for _, command := range commands {
			if verdict := s.applyPush(repo, command); verdict == "" {
				reportLines = append(reportLines, "ok "+command.ref+"\n")
			} else {
				reportLines = append(reportLines, "ng "+command.ref+" "+verdict+"\n")
			}
		}
	} else {
		reportLines = append(reportLines, "unpack "+unpackDetail+"\n")
	}
	// The report-status stream is itself pkt-lined inside band 1,
	// INCLUDING its terminating flush (a real git server sends
	// `...ok <ref>\n0000` inside the band-1 data, then a bare flush after
	// the sideband packets).
	var report bytes.Buffer
	for _, line := range reportLines {
		report.Write(encodePktLineString(line))
	}
	report.Write(pktFlush)
	var body bytes.Buffer
	if sideband {
		for _, part := range sidebandPackets(report.Bytes()) {
			body.Write(part)
		}
		body.Write(pktFlush)
	} else {
		// Without sideband the body is the report-status pkt-line stream
		// itself, which already ends with its terminating flush.
		body.Write(report.Bytes())
	}
	return httpd.Bytes(200, body.Bytes(), receiveResult)
}

// applyPush applies one ref update; it returns "" on success or the
// refusal reason. A delete never checks the old tip against the current
// one, matching the reference.
func (s *gitServer) applyPush(repo *store.RepoRecord, command pushCommand) string {
	current, exists := repo.Refs[command.ref]
	if command.newOID == util.ZeroOID {
		// Delete.
		if !exists {
			return "remote ref does not exist"
		}
		delete(repo.Refs, command.ref)
		return ""
	}
	if command.oldOID == util.ZeroOID {
		// Create.
		if exists {
			return "already exists"
		}
		if _, ok := repo.Objects[command.newOID]; !ok {
			return "object not found"
		}
		repo.Refs[command.ref] = command.newOID
		return ""
	}
	if !exists {
		return "remote ref does not exist"
	}
	if current != command.oldOID {
		return "non-fast-forward"
	}
	if command.newOID == command.oldOID {
		return ""
	}
	if _, ok := repo.Objects[command.newOID]; !ok {
		return "object not found"
	}
	if !s.store.IsAncestor(repo, command.oldOID, command.newOID) {
		return "non-fast-forward"
	}
	repo.Refs[command.ref] = command.newOID
	return ""
}
