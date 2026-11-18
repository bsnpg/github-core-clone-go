// Package gitobj implements git's object model from the protocol itself:
// the SHA-1 object ids, the commit/tree/blob/tag formats, and the
// reachability walks over them.
//
// No git binary is involved anywhere: the forge stores objects as bytes
// and re-emits them byte-for-byte.
package gitobj

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// ObjectType is the kind of a git object.
type ObjectType string

// The four git object types.
const (
	Commit ObjectType = "commit"
	Tree   ObjectType = "tree"
	Blob   ObjectType = "blob"
	Tag    ObjectType = "tag"
)

// GitObject is one stored git object: its type and its raw bytes.
type GitObject struct {
	Type ObjectType
	Data []byte
}

// CommitInfo is the parsed head of a commit object.
type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// EmptyTreeOID is the id of a tree object with no entries.
const EmptyTreeOID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Lookup resolves an object id to its object, or nil when it is unknown.
type Lookup func(oid string) *GitObject

// HashObject returns the SHA-1 of an object, exactly as git computes it:
// sha1("<type> <size>\0" + bytes).
func HashObject(typ ObjectType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// BuildCommit renders a commit object's bytes from its parts. Author and
// committer are full ident lines ("Name <email> <epoch> +0000").
func BuildCommit(tree string, parents []string, author, committer, message string) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, parent := range parents {
		b.WriteString("parent " + parent + "\n")
	}
	b.WriteString("author " + author + "\n")
	b.WriteString("committer " + committer + "\n")
	b.WriteString("\n" + message)
	return []byte(b.String())
}

// ParseCommit parses a commit object's bytes. The message is everything
// after the first blank line; without a blank line it is empty.
func ParseCommit(data []byte) CommitInfo {
	text := string(data)
	head, message := text, ""
	if i := strings.Index(text, "\n\n"); i >= 0 {
		head, message = text[:i], text[i+2:]
	}
	var info CommitInfo
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			info.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			info.Parents = append(info.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			info.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			info.Committer = line[10:]
		}
	}
	info.Message = message
	return info
}

// ParseTree parses a tree object's bytes into entries, in stored order. A
// malformed entry stops the parse; the entries read so far are returned.
func ParseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			break
		}
		mode := string(data[offset : offset+space])
		rest := offset + space + 1
		nul := bytes.IndexByte(data[rest:], 0x00)
		if nul < 0 {
			break
		}
		name := string(data[rest : rest+nul])
		oidStart := rest + nul + 1
		if oidStart+20 > len(data) {
			break
		}
		oid := hex.EncodeToString(data[oidStart : oidStart+20])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		offset = oidStart + 20
	}
	return entries
}

// treeSortKey is the comparison key git sorts tree entries by: a directory
// entry compares as if its name ended in "/".
func treeSortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

// BuildTree renders a tree object's bytes from entries, sorted git-style
// (directories compare as if ending in "/").
func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		fmt.Fprintf(&buf, "%s %s\x00", e.Mode, e.Name)
		raw, err := hex.DecodeString(e.OID)
		if err != nil {
			raw = make([]byte, 20)
		}
		buf.Write(raw)
	}
	return buf.Bytes()
}

// ParseTagTarget returns the object a tag points at and that object's type.
func ParseTagTarget(data []byte) (object string, typ string) {
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			typ = strings.TrimSpace(line[5:])
		case line == "":
			return object, typ
		}
	}
	return object, typ
}

// FlattenTree returns a flat map of path -> blob oid for the tree at
// treeOID, recursing into subtrees. A missing or non-tree object
// contributes nothing.
func FlattenTree(treeOID string, lookup Lookup) map[string]string {
	out := map[string]string{}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		obj := lookup(oid)
		if obj == nil || obj.Type != Tree {
			return
		}
		for _, e := range ParseTree(obj.Data) {
			path := e.Name
			if prefix != "" {
				path = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				walk(e.OID, path)
			} else {
				out[path] = e.OID
			}
		}
	}
	walk(treeOID, "")
	return out
}

// CollectCommits returns the commit ids reachable from roots (parents
// only), in discovery order, newest first by that order. A root that is
// missing or is not a commit contributes nothing.
func CollectCommits(roots []string, lookup Lookup) []string {
	var order []string
	seen := map[string]bool{}
	queue := append([]string(nil), roots...)
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil || obj.Type != Commit {
			continue
		}
		order = append(order, oid)
		queue = append(queue, ParseCommit(obj.Data).Parents...)
	}
	return order
}

// CollectReachable returns every object reachable from roots (commits and
// their trees and blobs, tags and their targets), keyed by oid. A missing
// object is skipped rather than failing the walk: unlike the Node port,
// which throws, this function has no error return, so a walk over a
// partially present graph yields the objects that are present.
func CollectReachable(roots []string, lookup Lookup) map[string]*GitObject {
	found := map[string]*GitObject{}
	seen := map[string]bool{}
	queue := append([]string(nil), roots...)
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		found[oid] = obj
		switch obj.Type {
		case Commit:
			commit := ParseCommit(obj.Data)
			queue = append(queue, commit.Tree)
			queue = append(queue, commit.Parents...)
		case Tree:
			for _, e := range ParseTree(obj.Data) {
				queue = append(queue, e.OID)
			}
		case Tag:
			target, _ := ParseTagTarget(obj.Data)
			queue = append(queue, target)
		}
	}
	return found
}

// IsAncestor reports whether ancestor is reachable from tip, or equal to
// it. The linear case — the tip's direct parent — is checked first with a
// single parse: every sequential push lands there, so the common path
// never walks history.
func IsAncestor(ancestor, tip string, lookup Lookup) bool {
	if ancestor == tip {
		return true
	}
	tipObj := lookup(tip)
	if tipObj == nil || tipObj.Type != Commit {
		return false
	}
	tipParents := ParseCommit(tipObj.Data).Parents
	for _, p := range tipParents {
		if p == ancestor {
			return true
		}
	}
	seen := map[string]bool{tip: true}
	queue := append([]string(nil), tipParents...)
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil || obj.Type != Commit {
			continue
		}
		for _, parent := range ParseCommit(obj.Data).Parents {
			if parent == ancestor {
				return true
			}
			queue = append(queue, parent)
		}
	}
	return false
}

// MergeBase returns the merge-base of two commits: the first common
// ancestor found by a joint walk. It returns "" when there is none.
func MergeBase(a, b string, lookup Lookup) string {
	if a == b {
		return a
	}
	ancestorsA := map[string]bool{}
	for _, oid := range CollectCommits([]string{a}, lookup) {
		ancestorsA[oid] = true
	}
	seen := map[string]bool{}
	queue := []string{b}
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if ancestorsA[oid] {
			return oid
		}
		obj := lookup(oid)
		if obj == nil || obj.Type != Commit {
			continue
		}
		queue = append(queue, ParseCommit(obj.Data).Parents...)
	}
	return ""
}
