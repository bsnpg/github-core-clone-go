package gitobj

import "testing"

// Known vectors, cross-checked against Python's hashlib (the same
// "<type> <len>\x00<data>" SHA-1 git uses) and against the Node port's
// EMPTY_TREE_OID constant.
func TestHashObjectVectors(t *testing.T) {
	cases := []struct {
		typ  ObjectType
		data []byte
		want string
	}{
		{Tree, []byte{}, "4b825dc642cb6eb9a060e54bf8d69288fbee4904"},
		{Blob, []byte{}, "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"},
		{Blob, []byte("hello\n"), "ce013625030ba8dba906f756967f9e9ca394464a"},
	}
	for _, c := range cases {
		if got := HashObject(c.typ, c.data); got != c.want {
			t.Errorf("HashObject(%s, %q) = %s, want %s", c.typ, c.data, got, c.want)
		}
	}
	if EmptyTreeOID != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Errorf("EmptyTreeOID = %s", EmptyTreeOID)
	}
}

func TestBuildCommitVector(t *testing.T) {
	ident := "ada iversen <adaiversen@example.invalid> 1699995200 +0000"
	data := BuildCommit(EmptyTreeOID, nil, ident, ident, "hello\n")
	if got := HashObject(Commit, data); got != "888ee1142eb233c0075a8f7d33df7fca491c2a88" {
		t.Errorf("commit hash = %s", got)
	}
	info := ParseCommit(data)
	if info.Tree != EmptyTreeOID || info.Author != ident || info.Committer != ident || info.Message != "hello\n" {
		t.Errorf("ParseCommit round-trip mismatch: %+v", info)
	}
	if len(info.Parents) != 0 {
		t.Errorf("expected no parents, got %v", info.Parents)
	}
}

func TestBuildTreeVector(t *testing.T) {
	blobOID := HashObject(Blob, []byte("hello\n"))
	data := BuildTree([]TreeEntry{{Mode: "100644", Name: "a.txt", OID: blobOID}})
	if got := HashObject(Tree, data); got != "2e81171448eb9f2ee3821e3d447aa6b2fe3ddba1" {
		t.Errorf("tree hash = %s", got)
	}
	entries := ParseTree(data)
	if len(entries) != 1 || entries[0].Name != "a.txt" || entries[0].OID != blobOID || entries[0].Mode != "100644" {
		t.Errorf("ParseTree round-trip mismatch: %+v", entries)
	}
}

func TestTreeSortsDirsWithTrailingSlash(t *testing.T) {
	// "a.txt" must sort before directory "a" (whose key is "a/"), even
	// though the bare names compare the other way round.
	entries := BuildTree([]TreeEntry{
		{Mode: "40000", Name: "a", OID: EmptyTreeOID},
		{Mode: "100644", Name: "a.txt", OID: HashObject(Blob, []byte("x"))},
	})
	parsed := ParseTree(entries)
	if len(parsed) != 2 || parsed[0].Name != "a.txt" || parsed[1].Name != "a" {
		t.Errorf("directory sort order wrong: %+v", parsed)
	}
}

func TestAncestorAndMergeBase(t *testing.T) {
	ident := "ada iversen <adaiversen@example.invalid> 1699995200 +0000"
	objects := map[string]*GitObject{}
	objects[EmptyTreeOID] = &GitObject{Type: Tree, Data: []byte{}}
	commit := func(parents []string, msg string) string {
		data := BuildCommit(EmptyTreeOID, parents, ident, ident, msg)
		oid := HashObject(Commit, data)
		objects[oid] = &GitObject{Type: Commit, Data: data}
		return oid
	}
	root := commit(nil, "root\n")
	mid := commit([]string{root}, "mid\n")
	tip := commit([]string{mid}, "tip\n")
	other := commit([]string{root}, "other\n")
	lookup := func(oid string) *GitObject { return objects[oid] }

	if !IsAncestor(root, tip, lookup) {
		t.Error("root should be an ancestor of tip")
	}
	if IsAncestor(tip, root, lookup) {
		t.Error("tip should not be an ancestor of root")
	}
	if got := MergeBase(tip, other, lookup); got != root {
		t.Errorf("MergeBase(tip, other) = %s, want %s", got, root)
	}
	if got := MergeBase(tip, tip, lookup); got != tip {
		t.Errorf("MergeBase(tip, tip) = %s, want %s", got, tip)
	}
	if got := MergeBase(root, commit(nil, "unrelated\n"), lookup); got != "" {
		t.Errorf("MergeBase of unrelated roots = %s, want empty", got)
	}
}
