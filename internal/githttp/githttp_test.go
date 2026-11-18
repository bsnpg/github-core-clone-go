// Tests for the git smart-HTTP surface. They drive the package through
// the real httpd router over a loopback listener, the way the forge
// serves it, and check wire bytes, pack contents and ref behaviour —
// including the fast-forward refusal and the pack cache.
package githttp

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"forge/internal/gitobj"
	"forge/internal/httpd"
	"forge/internal/store"
	"forge/internal/util"
)

// testServer is a live forge serving only the git routes.
type testServer struct {
	base  string
	store *store.Store
	ln    net.Listener
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	st := store.NewStore()
	r := &httpd.Router{}
	Register(r, st)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = r.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close() })
	return &testServer{base: "http://" + ln.Addr().String(), store: st, ln: ln}
}

func (s *testServer) get(t *testing.T, path string, headers map[string]string) (int, string, []byte) {
	t.Helper()
	req, err := http.NewRequest("GET", s.base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), body
}

func (s *testServer) post(t *testing.T, path string, body []byte, headers map[string]string) (int, string, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", s.base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), out
}

// seedRepo builds a repo with a 3-commit main branch, a feature branch
// off commit 2, and an annotated tag on commit 2. All timestamps are
// fixed, so the ids are deterministic.
func seedRepo(t *testing.T, st *store.Store) *store.RepoRecord {
	t.Helper()
	repo := st.CreateRepo("adaiversen", "demo", "", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}
	ident := "Test User <test@example.invalid> 1700000000 +0000"
	put := func(typ gitobj.ObjectType, data []byte) string {
		return st.PutObject(repo, typ, data)
	}
	blob := func(text string) string { return put(gitobj.Blob, []byte(text)) }
	tree := func(entries []gitobj.TreeEntry) string {
		return put(gitobj.Tree, gitobj.BuildTree(entries))
	}
	commit := func(treeOID string, parents []string, message string) string {
		return put(gitobj.Commit, gitobj.BuildCommit(treeOID, parents, ident, ident, message))
	}
	// Commit 1: one file, 1 KiB of 'a'.
	big := strings.Repeat("a", 1024)
	c1 := commit(tree([]gitobj.TreeEntry{{Mode: "100644", Name: "file.txt", OID: blob(big)}}), nil, "first\n")
	// Commit 2: file grows, second file appears.
	big2 := big + strings.Repeat("b", 512)
	c2 := commit(tree([]gitobj.TreeEntry{
		{Mode: "100644", Name: "file.txt", OID: blob(big2)},
		{Mode: "100644", Name: "other.txt", OID: blob("hello\n")},
	}), []string{c1}, "second\n")
	// Commit 3: file changes again.
	c3 := commit(tree([]gitobj.TreeEntry{
		{Mode: "100644", Name: "file.txt", OID: blob(big2 + "c")},
		{Mode: "100644", Name: "other.txt", OID: blob("hello\n")},
	}), []string{c2}, "third\n")
	repo.Refs["refs/heads/main"] = c3
	repo.Refs["refs/heads/feature"] = c2
	// Annotated tag v1 on commit 2.
	tagData := fmt.Sprintf("object %s\ntype commit\ntag v1\ntagger %s\n\nversion 1\n", c2, ident)
	tagOID := put(gitobj.Tag, []byte(tagData))
	repo.Refs["refs/tags/v1"] = tagOID
	return repo
}

// pkt joins payloads into one pkt-line stream; "flush"/"delim" are
// control packets.
func pkt(items ...any) []byte {
	var buf bytes.Buffer
	for _, item := range items {
		switch v := item.(type) {
		case string:
			buf.Write(encodePktLineString(v))
		case []byte:
			buf.Write(encodePktLine(v))
		case packetKind:
			if v == packetFlush {
				buf.Write(pktFlush)
			} else if v == packetDelim {
				buf.Write(pktDelim)
			}
		}
	}
	return buf.Bytes()
}

// unpackSideband extracts the pack bytes from a fetch response body:
// every data packet after the "packfile" marker (v2) — or after the
// leading NAK/ACK lines (v0) — is a band-1 packet.
func unpackSideband(t *testing.T, body []byte) []byte {
	t.Helper()
	packets, err := decodePktLines(body)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var pack []byte
	for _, p := range packets {
		if p.kind != packetData {
			continue
		}
		// A sideband packet is exactly one whose first payload byte is
		// the band byte 0x01; every section/ACK/shallow line is ASCII
		// text and never starts with it. This holds for both v0 (no
		// packfile marker) and v2 (sections precede the marker).
		if len(p.payload) == 0 || p.payload[0] != 1 {
			continue
		}
		pack = append(pack, p.payload[1:]...)
	}
	return pack
}

func TestInfoRefsV0(t *testing.T) {
	s := newTestServer(t)
	repo := seedRepo(t, s.store)
	mainTip := repo.Refs["refs/heads/main"]

	status, ctype, body := s.get(t, "/adaiversen/demo.git/info/refs?service=git-upload-pack", nil)
	if status != 200 || ctype != uploadAdvertisement {
		t.Fatalf("upload ad: status %d ctype %q", status, ctype)
	}
	text := string(body)
	if !strings.HasPrefix(text, "001e# service=git-upload-pack\n0000") {
		t.Fatalf("upload ad does not open with the preamble + flush: %q", text[:64])
	}
	if !strings.Contains(text, "side-band-64k") {
		t.Fatalf("upload ad does not advertise side-band-64k: %q", text)
	}
	if !strings.Contains(text, mainTip+" HEAD\x00") {
		t.Fatalf("upload ad has no HEAD line for %s", mainTip)
	}
	if !strings.Contains(text, "symref=HEAD:refs/heads/main") {
		t.Fatalf("upload ad has no symref: %q", text)
	}
	if !strings.HasSuffix(text, "0000") {
		t.Fatalf("upload ad does not end with a flush")
	}

	status, ctype, body = s.get(t, "/adaiversen/demo.git/info/refs?service=git-receive-pack", nil)
	if status != 200 || ctype != receiveAdvertisement {
		t.Fatalf("receive ad: status %d ctype %q", status, ctype)
	}
	text = string(body)
	if !strings.HasPrefix(text, "001f# service=git-receive-pack\n0000") {
		t.Fatalf("receive ad does not open with the preamble + flush: %q", text[:64])
	}
	for _, cap := range []string{"report-status", "side-band-64k", "delete-refs"} {
		if !strings.Contains(text, cap) {
			t.Fatalf("receive ad does not advertise %s", cap)
		}
	}

	// Missing repo: 404 on both services, server stays up.
	for _, service := range []string{"git-upload-pack", "git-receive-pack"} {
		status, _, body := s.get(t, "/adaiversen/nope.git/info/refs?service="+service, nil)
		if status != 404 {
			t.Fatalf("missing repo %s: status %d, want 404 (%s)", service, status, body)
		}
	}
	status, _, _ = s.get(t, "/adaiversen/demo.git/info/refs?service=git-upload-pack", nil)
	if status != 200 {
		t.Fatalf("server did not stay up after 404s: status %d", status)
	}

	// Missing service: 400, matching the reference dispatcher.
	status, _, _ = s.get(t, "/adaiversen/demo.git/info/refs", nil)
	if status != 400 {
		t.Fatalf("missing service: status %d, want 400", status)
	}
}

func TestInfoRefsV2(t *testing.T) {
	s := newTestServer(t)
	seedRepo(t, s.store)
	status, ctype, body := s.get(t, "/adaiversen/demo.git/info/refs?service=git-upload-pack",
		map[string]string{"Git-Protocol": "version=2"})
	if status != 200 || ctype != uploadAdvertisement {
		t.Fatalf("v2 ad: status %d ctype %q", status, ctype)
	}
	want := pkt("version 2\n", "agent=forge/1.0.0\n", "ls-refs=unborn\n",
		"fetch=shallow filter\n", "object-format=sha1\n", packetFlush)
	if !bytes.Equal(body, want) {
		t.Fatalf("v2 ad mismatch:\n got %q\nwant %q", body, want)
	}
	// Without the v2 header the same URL answers v0.
	status, _, body = s.get(t, "/adaiversen/demo.git/info/refs?service=git-upload-pack", nil)
	if status != 200 || !strings.HasPrefix(string(body), "001e# service=git-upload-pack\n") {
		t.Fatalf("v0 fallback: status %d body %q", status, body[:32])
	}
}

func TestLsRefsV2(t *testing.T) {
	s := newTestServer(t)
	repo := seedRepo(t, s.store)
	body := pkt("command=ls-refs\n", packetDelim, "peel\n", "symrefs\n", "unborn\n", packetFlush)
	status, ctype, out := s.post(t, "/adaiversen/demo.git/git-upload-pack", body,
		map[string]string{"Git-Protocol": "version=2"})
	if status != 200 || ctype != uploadResult {
		t.Fatalf("ls-refs: status %d ctype %q (%s)", status, ctype, out)
	}
	text := string(out)
	mainTip := repo.Refs["refs/heads/main"]
	if !strings.Contains(text, mainTip+" HEAD symref-target:refs/heads/main\n") {
		t.Fatalf("ls-refs has no symref HEAD line: %q", text)
	}
	if !strings.Contains(text, repo.Refs["refs/tags/v1"]+" refs/tags/v1 peeled:"+repo.Refs["refs/heads/feature"]+"\n") {
		t.Fatalf("ls-refs has no peeled tag line: %q", text)
	}
	if !strings.HasSuffix(text, "0000") {
		t.Fatalf("ls-refs does not end with a flush")
	}
	// Prefix filter keeps only matching refs.
	body = pkt("command=ls-refs\n", packetDelim, "ref-prefix refs/heads/\n", packetFlush)
	_, _, out = s.post(t, "/adaiversen/demo.git/git-upload-pack", body, nil)
	if strings.Contains(string(out), "refs/tags/") {
		t.Fatalf("ref-prefix did not filter tags out: %q", out)
	}
}

func TestEmptyRepo(t *testing.T) {
	s := newTestServer(t)
	repo := s.store.CreateRepo("adaiversen", "empty", "", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}
	status, _, body := s.get(t, "/adaiversen/empty.git/info/refs?service=git-upload-pack", nil)
	if status != 200 {
		t.Fatalf("empty repo ad: status %d", status)
	}
	if !strings.Contains(string(body), "0000000000000000000000000000000000000000 capabilities^{}\x00") {
		t.Fatalf("empty repo ad has no zero-id capabilities line: %q", body)
	}
	// ls-refs with unborn reports the unborn HEAD.
	req := pkt("command=ls-refs\n", packetDelim, "unborn\n", "symrefs\n", packetFlush)
	_, _, out := s.post(t, "/adaiversen/empty.git/git-upload-pack", req, nil)
	if !strings.Contains(string(out), "unborn HEAD symref-target:refs/heads/main\n") {
		t.Fatalf("ls-refs unborn has no unborn HEAD line: %q", out)
	}
	// Without unborn, ls-refs on an empty repo answers just a flush.
	req = pkt("command=ls-refs\n", packetDelim, packetFlush)
	_, _, out = s.post(t, "/adaiversen/empty.git/git-upload-pack", req, nil)
	if string(out) != "0000" {
		t.Fatalf("ls-refs on empty repo without unborn: %q, want just a flush", out)
	}
}

func TestFetchV2Shallow(t *testing.T) {
	s := newTestServer(t)
	repo := seedRepo(t, s.store)
	tip := repo.Refs["refs/heads/main"]
	body := pkt("command=fetch\n", packetDelim,
		"object-format=sha1\n", "want "+tip+"\n", "deepen 1\n", "done\n", packetFlush)
	status, ctype, out := s.post(t, "/adaiversen/demo.git/git-upload-pack", body,
		map[string]string{"Git-Protocol": "version=2"})
	if status != 200 || ctype != uploadResult {
		t.Fatalf("fetch v2: status %d ctype %q (%s)", status, ctype, out)
	}
	text := string(out)
	if !strings.Contains(text, "shallow-info\n") || !strings.Contains(text, "shallow "+tip+"\n") {
		t.Fatalf("shallow fetch has no shallow-info for the tip: %q", text)
	}
	if strings.Contains(text, "acknowledgments\n") {
		t.Fatalf("a `done` fetch must not open a negotiation section: %q", text)
	}
	pack := unpackSideband(t, out)
	objects, err := readPack(pack, s.store.Lookup(repo))
	if err != nil {
		t.Fatalf("readPack of own fetch: %v", err)
	}
	// Depth 1: the tip commit, its tree and its blobs only.
	commit := repo.Objects[tip]
	treeOID := gitobj.ParseCommit(commit.Data).Tree
	want := map[string]bool{tip: true, treeOID: true}
	for _, e := range gitobj.ParseTree(repo.Objects[treeOID].Data) {
		want[e.OID] = true
	}
	if len(objects) != len(want) {
		t.Fatalf("shallow pack holds %d objects, want %d: %v", len(objects), len(want), keys(objects))
	}
	for oid := range want {
		if _, ok := objects[oid]; !ok {
			t.Fatalf("shallow pack is missing %s", oid)
		}
	}
	// The pack trailer is the SHA-1 of everything before it.
	if got := pack[len(pack)-20:]; !bytes.Equal(got, mustSHA1(t, pack[:len(pack)-20])) {
		t.Fatalf("pack trailer mismatch")
	}
}

func TestFetchV2NoDoneNegotiates(t *testing.T) {
	s := newTestServer(t)
	repo := seedRepo(t, s.store)
	tip := repo.Refs["refs/heads/main"]
	c2 := repo.Refs["refs/heads/feature"]
	// No `done`: the client still negotiates, so the answer opens with
	// acknowledgments and wanted-refs.
	body := pkt("command=fetch\n", packetDelim, "want "+tip+"\n", "have "+c2+"\n", packetFlush)
	_, _, out := s.post(t, "/adaiversen/demo.git/git-upload-pack", body, nil)
	text := string(out)
	if !strings.Contains(text, "acknowledgments\n") || !strings.Contains(text, "ACK "+c2+"\n") {
		t.Fatalf("negotiating fetch has no ACK section: %q", text)
	}
	if !strings.Contains(text, "wanted-refs\n") || !strings.Contains(text, tip+" refs/heads/main\n") {
		t.Fatalf("negotiating fetch has no wanted-refs: %q", text)
	}
	// The client already has commit 2 and its closure, so the pack must
	// not carry commit 2 or its tree/blobs — only commit 3's own objects.
	pack := unpackSideband(t, out)
	objects, err := readPack(pack, s.store.Lookup(repo))
	if err != nil {
		t.Fatalf("readPack: %v", err)
	}
	if _, ok := objects[c2]; ok {
		t.Fatalf("pack carries a commit the client already has")
	}
	if _, ok := objects[tip]; !ok {
		t.Fatalf("pack is missing the wanted tip")
	}
}

func TestFetchV0(t *testing.T) {
	s := newTestServer(t)
	repo := seedRepo(t, s.store)
	tip := repo.Refs["refs/heads/main"]
	body := pkt("want "+tip+"\n", "done\n")
	status, ctype, out := s.post(t, "/adaiversen/demo.git/git-upload-pack", body, nil)
	if status != 200 || ctype != uploadResult {
		t.Fatalf("fetch v0: status %d ctype %q (%s)", status, ctype, out)
	}
	if !strings.HasPrefix(string(out), "0008NAK\n") {
		t.Fatalf("v0 fetch does not open with NAK: %q", out[:32])
	}
	pack := unpackSideband(t, out)
	objects, err := readPack(pack, s.store.Lookup(repo))
	if err != nil {
		t.Fatalf("readPack of v0 fetch: %v", err)
	}
	// Full history reachable from the tip: 3 commits + 3 trees + 4 blobs
	// = 10 objects. (The annotated tag is not reachable from the tip —
	// it is only sent when it is itself wanted.)
	if len(objects) != 10 {
		t.Fatalf("v0 full pack holds %d objects, want 10: %v", len(objects), keys(objects))
	}
	// Wanting the tag itself sends the tag plus its target's closure.
	tagOID := repo.Refs["refs/tags/v1"]
	body = pkt("want "+tagOID+"\n", "done\n")
	_, _, out = s.post(t, "/adaiversen/demo.git/git-upload-pack", body, nil)
	pack = unpackSideband(t, out)
	objects, err = readPack(pack, s.store.Lookup(repo))
	if err != nil {
		t.Fatalf("readPack of v0 tag fetch: %v", err)
	}
	if _, ok := objects[tagOID]; !ok {
		t.Fatalf("tag fetch pack is missing the tag")
	}
}

func TestFetchCacheIdentical(t *testing.T) {
	s := newTestServer(t)
	repo := seedRepo(t, s.store)
	tip := repo.Refs["refs/heads/main"]
	body := pkt("command=fetch\n", packetDelim, "want "+tip+"\n", "deepen 1\n", "done\n", packetFlush)
	_, _, first := s.post(t, "/adaiversen/demo.git/git-upload-pack", body, nil)
	_, _, second := s.post(t, "/adaiversen/demo.git/git-upload-pack", body, nil)
	if !bytes.Equal(first, second) {
		t.Fatalf("identical fetches answered different bytes")
	}
}

func TestPackRoundTrip(t *testing.T) {
	s := newTestServer(t)
	repo := seedRepo(t, s.store)
	var objects []*gitobj.GitObject
	for _, oid := range []string{
		repo.Refs["refs/heads/main"],
		repo.Refs["refs/heads/feature"],
		repo.Refs["refs/tags/v1"],
	} {
		objects = append(objects, repo.Objects[oid])
	}
	pack := writePack(objects)
	got, err := readPack(pack, s.store.Lookup(repo))
	if err != nil {
		t.Fatalf("readPack(writePack): %v", err)
	}
	if len(got) != len(objects) {
		t.Fatalf("round trip holds %d objects, want %d", len(got), len(objects))
	}
	for _, obj := range objects {
		oid := gitobj.HashObject(obj.Type, obj.Data)
		back, ok := got[oid]
		if !ok || back.Type != obj.Type || !bytes.Equal(back.Data, obj.Data) {
			t.Fatalf("round trip mismatch for %s", oid)
		}
	}
	// A bit flip in the trailer must fail the checksum.
	bad := append([]byte(nil), pack...)
	bad[len(bad)-1] ^= 0xff
	if _, err := readPack(bad, s.store.Lookup(repo)); err == nil {
		t.Fatalf("corrupt trailer passed")
	}
}

// pushRequest builds a receive-pack body: command pkt-lines, a flush,
// then a whole-object pack of the given objects.
func pushRequest(t *testing.T, commands []string, objects []*gitobj.GitObject) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, cmd := range commands {
		buf.Write(encodePktLineString(cmd))
	}
	buf.Write(pktFlush)
	if len(objects) > 0 {
		buf.Write(writePack(objects))
	}
	return buf.Bytes()
}

func TestReceivePackFastForward(t *testing.T) {
	s := newTestServer(t)
	repo := seedRepo(t, s.store)
	oldTip := repo.Refs["refs/heads/main"]
	ident := "Test User <test@example.invalid> 1700000000 +0000"
	// A child of the tip on main.
	treeOID := gitobj.ParseCommit(repo.Objects[oldTip].Data).Tree
	newCommitData := gitobj.BuildCommit(treeOID, []string{oldTip}, ident, ident, "fourth\n")
	newCommit := &gitobj.GitObject{Type: gitobj.Commit, Data: newCommitData}
	newOID := gitobj.HashObject(gitobj.Commit, newCommitData)
	cmd := oldTip + " " + newOID + " refs/heads/main\x00side-band-64k report-status\n"
	status, ctype, out := s.post(t, "/adaiversen/demo.git/git-receive-pack",
		pushRequest(t, []string{cmd}, []*gitobj.GitObject{newCommit}), nil)
	if status != 200 || ctype != receiveResult {
		t.Fatalf("receive-pack: status %d ctype %q (%s)", status, ctype, out)
	}
	report := decodeReport(t, out)
	if !strings.Contains(report, "unpack ok\n") || !strings.Contains(report, "ok refs/heads/main\n") {
		t.Fatalf("fast-forward push not accepted: %q", report)
	}
	if repo.Refs["refs/heads/main"] != newOID {
		t.Fatalf("ref did not move: %s", repo.Refs["refs/heads/main"])
	}
	if repo.Objects[newOID] == nil {
		t.Fatalf("pushed object was not stored")
	}
}

func TestReceivePackNonFastForward(t *testing.T) {
	s := newTestServer(t)
	repo := seedRepo(t, s.store)
	oldTip := repo.Refs["refs/heads/main"]
	featureTip := repo.Refs["refs/heads/feature"]
	ident := "Test User <test@example.invalid> 1700000000 +0000"
	// A divergent commit: child of the feature tip, pushed onto main.
	treeOID := gitobj.ParseCommit(repo.Objects[featureTip].Data).Tree
	divData := gitobj.BuildCommit(treeOID, []string{featureTip}, ident, ident, "divergent\n")
	divergent := &gitobj.GitObject{Type: gitobj.Commit, Data: divData}
	divOID := gitobj.HashObject(gitobj.Commit, divData)
	cmd := oldTip + " " + divOID + " refs/heads/main\x00side-band-64k report-status\n"
	_, _, out := s.post(t, "/adaiversen/demo.git/git-receive-pack",
		pushRequest(t, []string{cmd}, []*gitobj.GitObject{divergent}), nil)
	report := decodeReport(t, out)
	if !strings.Contains(report, "ng refs/heads/main non-fast-forward\n") {
		t.Fatalf("non-fast-forward push not refused per ref: %q", report)
	}
	if repo.Refs["refs/heads/main"] != oldTip {
		t.Fatalf("ref moved on a refused push: %s", repo.Refs["refs/heads/main"])
	}
	// The object itself was still stored (unpack runs before ref checks).
	if repo.Objects[divOID] == nil {
		t.Fatalf("unpacked object was not stored")
	}
}

func TestReceivePackMixed(t *testing.T) {
	s := newTestServer(t)
	repo := seedRepo(t, s.store)
	mainTip := repo.Refs["refs/heads/main"]
	featureTip := repo.Refs["refs/heads/feature"]
	ident := "Test User <test@example.invalid> 1700000000 +0000"
	// One fast-forward (feature advances to main's tip) and one refused
	// (main to a divergent commit) in a single push.
	treeOID := gitobj.ParseCommit(repo.Objects[featureTip].Data).Tree
	divData := gitobj.BuildCommit(treeOID, []string{featureTip}, ident, ident, "divergent\n")
	divergent := &gitobj.GitObject{Type: gitobj.Commit, Data: divData}
	divOID := gitobj.HashObject(gitobj.Commit, divData)
	cmds := []string{
		featureTip + " " + mainTip + " refs/heads/feature\x00side-band-64k report-status\n",
		mainTip + " " + divOID + " refs/heads/main\n",
	}
	_, _, out := s.post(t, "/adaiversen/demo.git/git-receive-pack",
		pushRequest(t, cmds, []*gitobj.GitObject{divergent}), nil)
	report := decodeReport(t, out)
	if !strings.Contains(report, "ok refs/heads/feature\n") {
		t.Fatalf("fast-forward ref not applied in mixed push: %q", report)
	}
	if !strings.Contains(report, "ng refs/heads/main non-fast-forward\n") {
		t.Fatalf("refused ref not reported in mixed push: %q", report)
	}
	if repo.Refs["refs/heads/feature"] != mainTip {
		t.Fatalf("fast-forward ref did not move in mixed push")
	}
	if repo.Refs["refs/heads/main"] != mainTip {
		t.Fatalf("refused ref moved in mixed push")
	}
}

func TestReceivePackMissingRepo(t *testing.T) {
	s := newTestServer(t)
	status, _, _ := s.post(t, "/adaiversen/nope.git/git-receive-pack", pkt("0000"), nil)
	if status != 404 {
		t.Fatalf("push to missing repo: status %d, want 404", status)
	}
	status, _, _ = s.post(t, "/adaiversen/nope.git/git-upload-pack", pkt("done\n"), nil)
	if status != 404 {
		t.Fatalf("fetch from missing repo: status %d, want 404", status)
	}
}

func TestReceivePackMalformed(t *testing.T) {
	s := newTestServer(t)
	seedRepo(t, s.store)
	status, _, _ := s.post(t, "/adaiversen/demo.git/git-receive-pack", []byte("zzzz"), nil)
	if status != 400 {
		t.Fatalf("malformed receive-pack: status %d, want 400", status)
	}
	status, _, _ = s.post(t, "/adaiversen/demo.git/git-upload-pack", []byte("zzzz"), nil)
	if status != 400 {
		t.Fatalf("malformed upload-pack: status %d, want 400", status)
	}
}

func TestConcurrentShallowClones(t *testing.T) {
	s := newTestServer(t)
	repo := seedRepo(t, s.store)
	tip := repo.Refs["refs/heads/main"]
	body := pkt("command=fetch\n", packetDelim, "want "+tip+"\n", "deepen 1\n", "done\n", packetFlush)
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _, out := s.post(t, "/adaiversen/demo.git/git-upload-pack", body, nil)
			if status != 200 {
				errs <- fmt.Errorf("status %d", status)
				return
			}
			if len(unpackSideband(t, out)) == 0 {
				errs <- fmt.Errorf("empty pack")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// decodeReport unwraps a sideband report-status body into its text.
func decodeReport(t *testing.T, body []byte) string {
	t.Helper()
	packets, err := decodePktLines(body)
	if err != nil {
		t.Fatalf("decode report: %v", err)
	}
	var raw []byte
	for _, p := range packets {
		if p.kind == packetData {
			if len(p.payload) == 0 || p.payload[0] != 1 {
				t.Fatalf("report packet with band %v", p.payload[:1])
			}
			raw = append(raw, p.payload[1:]...)
		}
	}
	return string(raw)
}

func keys(m map[string]*gitobj.GitObject) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustSHA1(t *testing.T, data []byte) []byte {
	t.Helper()
	digest := util.SHA1Digest(data)
	return digest[:]
}
