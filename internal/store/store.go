// Package store holds the forge's whole state, in memory.
//
// One process, one Store. Repositories hold their own git objects and refs
// plus their issues, pull requests, labels and comments. Nothing here
// touches the disk, the network or the clock: all timestamps come from the
// trial's fixed "now", all ids from counters, so the same requests in the
// same order always build the same state.
package store

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"forge/internal/gitobj"
	"forge/internal/util"
)

// Viewer is the token owner's identity: the scenario's viewer, fixed by
// the kit's seed.
var Viewer = struct {
	ID    int
	Login string
	Name  string
	Email string
}{ID: 1000, Login: "adaiversen", Name: "ada iversen", Email: "adaiversen@example.invalid"}

// ForgeIdent returns the git ident line every commit this forge authors
// carries.
func ForgeIdent() string {
	return fmt.Sprintf("%s <%s> %d +0000", Viewer.Name, Viewer.Email, util.TrialNowEpoch())
}

// LabelRecord is one repository label.
type LabelRecord struct {
	Name        string
	Color       string
	Description string
}

// CommentRecord is one comment on an issue.
type CommentRecord struct {
	ID        int
	Body      string
	User      string
	CreatedAt string
}

// IssueRecord is one issue. ViewBuf caches the REST single-issue JSON
// body: the read workload asks for the same issue thousands of times and
// nothing about it changes between reads. It is cleared by any mutation
// of the issue. The bytes are shared read-only — callers copy them into
// each response, never mutate them.
type IssueRecord struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // "open" or "closed"
	Labels    []string
	User      string
	CreatedAt string
	UpdatedAt string
	Comments  []*CommentRecord
	ViewBuf   []byte
}

// PullRecord is one pull request.
type PullRecord struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" or "closed"
	Merged         bool
	MergedAt       *string
	MergeCommitSha *string
	HeadRef        string
	HeadSha        string
	BaseRef        string
	BaseSha        string
	User           string
	CreatedAt      string
	UpdatedAt      string
}

// RepoRecord is one repository and everything inside it.
type RepoRecord struct {
	ID             int
	Owner          string
	Name           string
	Description    string
	Private        bool
	DefaultBranch  string
	CreatedAt      string
	UpdatedAt      string
	Objects        map[string]*gitobj.GitObject
	Refs           map[string]string
	Labels         map[string]*LabelRecord
	Issues         []*IssueRecord
	Pulls          []*PullRecord
	NextNumber     int
	FlatTreeCache  map[string]map[string]string
	IssuesByNumber map[int]*IssueRecord
	PullsByNumber  map[int]*PullRecord
}

// RepoKey is the map key for a repository: owner and name, lowercased.
func RepoKey(owner, name string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(name)
}

// FileChange is one changed path between two trees, with line counts.
type FileChange struct {
	Filename  string
	Status    string // "added", "modified" or "removed"
	Additions int
	Deletions int
	Changes   int
	OldOID    *string
	NewOID    *string
}

// CommitHistoryEntry is one entry of a commit history: the sha and the
// parsed commit.
type CommitHistoryEntry struct {
	SHA    string
	Commit gitobj.CommitInfo
}

// ContentCommitResult is what ContentCommit returns: the new commit's sha
// and, for a write, the new blob's sha (empty when the path was deleted).
type ContentCommitResult struct {
	CommitSHA  string
	ContentSHA string
}

// DecodedGlobalID is the result of DecodeGlobalID. Kind is one of
// "repo", "issue", "pull" or "comment"; only the fields that kind carries
// are set.
type DecodedGlobalID struct {
	Kind    string
	Repo    *RepoRecord
	Issue   *IssueRecord
	Pull    *PullRecord
	Comment *CommentRecord
}

// Store is the forge's whole state.
type Store struct {
	Repos       map[string]*RepoRecord
	reposByID   map[int]*RepoRecord
	nextRepoID  int
	nextObjID   int
	nextComment int
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{
		Repos:       map[string]*RepoRecord{},
		reposByID:   map[int]*RepoRecord{},
		nextRepoID:  1,
		nextObjID:   1,
		nextComment: 1,
	}
}

// ensureInit repairs a zero-value Store (built without NewStore) so its
// methods still work.
func (s *Store) ensureInit() {
	if s.Repos == nil {
		s.Repos = map[string]*RepoRecord{}
	}
	if s.reposByID == nil {
		s.reposByID = map[int]*RepoRecord{}
	}
	if s.nextRepoID == 0 {
		s.nextRepoID = 1
	}
	if s.nextObjID == 0 {
		s.nextObjID = 1
	}
	if s.nextComment == 0 {
		s.nextComment = 1
	}
}

// stockLabels are the labels every GitHub repository starts with.
var stockLabels = [][2]string{
	{"bug", "d73a4a"},
	{"enhancement", "a2eeef"},
	{"documentation", "0075ca"},
	{"question", "d876e3"},
	{"good first issue", "7057ff"},
	{"help wanted", "008672"},
}

// CreateRepo creates a repository, or returns nil when one with that
// owner and name already exists.
func (s *Store) CreateRepo(owner, name, description string, private bool) *RepoRecord {
	s.ensureInit()
	key := RepoKey(owner, name)
	if _, exists := s.Repos[key]; exists {
		return nil
	}
	repo := &RepoRecord{
		ID:             s.nextRepoID,
		Owner:          strings.ToLower(owner),
		Name:           name,
		Description:    description,
		Private:        private,
		DefaultBranch:  "main",
		CreatedAt:      util.TrialNowISO(),
		UpdatedAt:      util.TrialNowISO(),
		Objects:        map[string]*gitobj.GitObject{},
		Refs:           map[string]string{},
		Labels:         map[string]*LabelRecord{},
		NextNumber:     1,
		FlatTreeCache:  map[string]map[string]string{},
		IssuesByNumber: map[int]*IssueRecord{},
		PullsByNumber:  map[int]*PullRecord{},
	}
	s.nextRepoID++
	for _, pair := range stockLabels {
		repo.Labels[pair[0]] = &LabelRecord{Name: pair[0], Color: pair[1], Description: ""}
	}
	s.Repos[key] = repo
	s.reposByID[repo.ID] = repo
	return repo
}

// GetRepo returns the repository, or nil when there is none.
func (s *Store) GetRepo(owner, name string) *RepoRecord {
	return s.Repos[RepoKey(owner, name)]
}

// DeleteRepo removes a repository; it reports whether one was there.
func (s *Store) DeleteRepo(owner, name string) bool {
	key := RepoKey(owner, name)
	repo, ok := s.Repos[key]
	if !ok {
		return false
	}
	delete(s.reposByID, repo.ID)
	delete(s.Repos, key)
	return true
}

// ListRepos returns one owner's repositories, sorted by id.
func (s *Store) ListRepos(owner string) []*RepoRecord {
	lower := strings.ToLower(owner)
	var out []*RepoRecord
	for _, repo := range s.Repos {
		if repo.Owner == lower {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RepoGlobalID returns a repository's opaque Relay global id.
func (s *Store) RepoGlobalID(repo *RepoRecord) string {
	return fmt.Sprintf("R%d", repo.ID)
}

// IssueGlobalID returns an issue's opaque Relay global id.
func (s *Store) IssueGlobalID(repo *RepoRecord, issue *IssueRecord) string {
	return fmt.Sprintf("I%d-%d", repo.ID, issue.Number)
}

// PullGlobalID returns a pull request's opaque Relay global id.
func (s *Store) PullGlobalID(repo *RepoRecord, pull *PullRecord) string {
	return fmt.Sprintf("P%d-%d", repo.ID, pull.Number)
}

// CommentGlobalID returns a comment's opaque Relay global id.
func (s *Store) CommentGlobalID(repo *RepoRecord, issue *IssueRecord, comment *CommentRecord) string {
	return fmt.Sprintf("C%d-%d-%d", repo.ID, issue.Number, comment.ID)
}

var (
	repoIDPattern    = regexp.MustCompile(`^R(\d+)$`)
	issueIDPattern   = regexp.MustCompile(`^I(\d+)-(\d+)$`)
	pullIDPattern    = regexp.MustCompile(`^P(\d+)-(\d+)$`)
	commentIDPattern = regexp.MustCompile(`^C(\d+)-(\d+)-(\d+)$`)
)

// DecodeGlobalID decodes an opaque Relay global id back into the records
// it names, or nil when it names nothing that exists.
func (s *Store) DecodeGlobalID(id string) *DecodedGlobalID {
	if m := repoIDPattern.FindStringSubmatch(id); m != nil {
		repo := s.reposByID[mustAtoi(m[1])]
		if repo == nil {
			return nil
		}
		return &DecodedGlobalID{Kind: "repo", Repo: repo}
	}
	if m := issueIDPattern.FindStringSubmatch(id); m != nil {
		repo := s.reposByID[mustAtoi(m[1])]
		if repo == nil {
			return nil
		}
		issue := s.GetIssue(repo, mustAtoi(m[2]))
		if issue == nil {
			return nil
		}
		return &DecodedGlobalID{Kind: "issue", Repo: repo, Issue: issue}
	}
	if m := pullIDPattern.FindStringSubmatch(id); m != nil {
		repo := s.reposByID[mustAtoi(m[1])]
		if repo == nil {
			return nil
		}
		pull := s.GetPull(repo, mustAtoi(m[2]))
		if pull == nil {
			return nil
		}
		return &DecodedGlobalID{Kind: "pull", Repo: repo, Pull: pull}
	}
	if m := commentIDPattern.FindStringSubmatch(id); m != nil {
		repo := s.reposByID[mustAtoi(m[1])]
		if repo == nil {
			return nil
		}
		issue := s.GetIssue(repo, mustAtoi(m[2]))
		if issue == nil {
			return nil
		}
		for _, comment := range issue.Comments {
			if comment.ID == mustAtoi(m[3]) {
				return &DecodedGlobalID{Kind: "comment", Repo: repo, Issue: issue, Comment: comment}
			}
		}
		return nil
	}
	return nil
}

func mustAtoi(text string) int {
	n, _ := strconv.Atoi(text)
	return n
}

// GetObject returns one of a repository's git objects, or nil.
func (s *Store) GetObject(repo *RepoRecord, oid string) *gitobj.GitObject {
	return repo.Objects[oid]
}

// PutObject stores an object (if it is not already there) and returns its id.
func (s *Store) PutObject(repo *RepoRecord, typ gitobj.ObjectType, data []byte) string {
	oid := gitobj.HashObject(typ, data)
	if _, exists := repo.Objects[oid]; !exists {
		repo.Objects[oid] = &gitobj.GitObject{Type: typ, Data: data}
	}
	return oid
}

// PutObjects stores every object in the map that is not already present.
func (s *Store) PutObjects(repo *RepoRecord, objects map[string]*gitobj.GitObject) {
	for oid, object := range objects {
		if _, exists := repo.Objects[oid]; !exists {
			repo.Objects[oid] = object
		}
	}
}

// Lookup returns a gitobj.Lookup over a repository's objects.
func (s *Store) Lookup(repo *RepoRecord) gitobj.Lookup {
	return func(oid string) *gitobj.GitObject {
		return repo.Objects[oid]
	}
}

// ResolveRef resolves a ref name, a branch name or a full oid to a commit
// oid, or "" when it resolves to nothing.
func (s *Store) ResolveRef(repo *RepoRecord, ref string) string {
	if util.IsOID(ref) {
		if _, exists := repo.Objects[ref]; exists {
			return ref
		}
		return ""
	}
	if strings.HasPrefix(ref, "refs/") {
		return repo.Refs[ref]
	}
	if sha, ok := repo.Refs["refs/heads/"+ref]; ok {
		return sha
	}
	return repo.Refs["refs/tags/"+ref]
}

// CommitHistory returns the commit history reachable from tip, newest
// first by discovery order.
func (s *Store) CommitHistory(repo *RepoRecord, tip string) []CommitHistoryEntry {
	oids := gitobj.CollectCommits([]string{tip}, s.Lookup(repo))
	var out []CommitHistoryEntry
	for _, oid := range oids {
		obj := repo.Objects[oid]
		if obj != nil && obj.Type == gitobj.Commit {
			out = append(out, CommitHistoryEntry{SHA: oid, Commit: gitobj.ParseCommit(obj.Data)})
		}
	}
	return out
}

// FlatTree returns a tree's flat path map, cached per tree oid on the repo.
func (s *Store) FlatTree(repo *RepoRecord, treeOID string) map[string]string {
	if cached, ok := repo.FlatTreeCache[treeOID]; ok {
		return cached
	}
	flat := gitobj.FlattenTree(treeOID, s.Lookup(repo))
	repo.FlatTreeCache[treeOID] = flat
	return flat
}

// BuildTreeFromFlat builds (and stores) the tree for a flat path map and
// returns the root tree oid.
func (s *Store) BuildTreeFromFlat(repo *RepoRecord, flat map[string]string) string {
	if len(flat) == 0 {
		if _, exists := repo.Objects[gitobj.EmptyTreeOID]; !exists {
			repo.Objects[gitobj.EmptyTreeOID] = &gitobj.GitObject{Type: gitobj.Tree, Data: []byte{}}
		}
		return gitobj.EmptyTreeOID
	}
	var build func(prefix string) string
	build = func(prefix string) string {
		var entries []gitobj.TreeEntry
		dirs := map[string]bool{}
		for path, oid := range flat {
			if prefix != "" && !strings.HasPrefix(path, prefix+"/") {
				continue
			}
			rest := path
			if prefix != "" {
				rest = path[len(prefix)+1:]
			}
			if slash := strings.Index(rest, "/"); slash == -1 {
				entries = append(entries, gitobj.TreeEntry{Mode: "100644", Name: rest, OID: oid})
			} else {
				dirs[rest[:slash]] = true
			}
		}
		for dir := range dirs {
			subPrefix := dir
			if prefix != "" {
				subPrefix = prefix + "/" + dir
			}
			entries = append(entries, gitobj.TreeEntry{Mode: "40000", Name: dir, OID: build(subPrefix)})
		}
		return s.PutObject(repo, gitobj.Tree, gitobj.BuildTree(entries))
	}
	return build("")
}

// ContentCommit creates a commit on branch that sets path to content (or
// deletes it when content is nil). It returns nil when the branch is
// unborn and is not the default branch, when the tip is not a commit, or
// when a delete names a path that is not there.
func (s *Store) ContentCommit(repo *RepoRecord, branch, path string, content *string, message string) *ContentCommitResult {
	refName := "refs/heads/" + branch
	tip, hasTip := repo.Refs[refName]
	if !hasTip && branch != repo.DefaultBranch {
		return nil
	}
	parentTreeOID := gitobj.EmptyTreeOID
	var parents []string
	if hasTip {
		commitObj := repo.Objects[tip]
		if commitObj == nil || commitObj.Type != gitobj.Commit {
			return nil
		}
		parentTreeOID = gitobj.ParseCommit(commitObj.Data).Tree
		parents = []string{tip}
	}
	flat := make(map[string]string, len(s.FlatTree(repo, parentTreeOID)))
	for k, v := range s.FlatTree(repo, parentTreeOID) {
		flat[k] = v
	}
	var contentSHA string
	if content == nil {
		if _, exists := flat[path]; !exists {
			return nil
		}
		delete(flat, path)
	} else {
		contentSHA = s.PutObject(repo, gitobj.Blob, []byte(*content))
		flat[path] = contentSHA
	}
	treeOID := s.BuildTreeFromFlat(repo, flat)
	ident := ForgeIdent()
	commitData := gitobj.BuildCommit(treeOID, parents, ident, ident, message)
	commitSHA := s.PutObject(repo, gitobj.Commit, commitData)
	repo.Refs[refName] = commitSHA
	return &ContentCommitResult{CommitSHA: commitSHA, ContentSHA: contentSHA}
}

// LineDiff returns a line diff's added and removed counts, from an LCS
// over lines. A trailing newline does not count as an extra line, so
// "a\n" and "a" count as the same single line.
func LineDiff(oldText, newText string) (additions, deletions int) {
	if oldText == newText {
		return 0, 0
	}
	var a, b []string
	if oldText != "" {
		a = strings.Split(oldText, "\n")
	}
	if newText != "" {
		b = strings.Split(newText, "\n")
	}
	if len(a) > 0 && a[len(a)-1] == "" {
		a = a[:len(a)-1]
	}
	if len(b) > 0 && b[len(b)-1] == "" {
		b = b[:len(b)-1]
	}
	n, m := len(a), len(b)
	if n == 0 {
		return m, 0
	}
	if m == 0 {
		return 0, n
	}
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] > cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
		for j := range cur {
			cur[j] = 0
		}
	}
	lcs := prev[m]
	return m - lcs, n - lcs
}

// LineDiff is the Store method form of the package-level LineDiff.
func (s *Store) LineDiff(oldText, newText string) (additions, deletions int) {
	return LineDiff(oldText, newText)
}

// DiffTrees returns the file changes between two trees (base -> head).
func (s *Store) DiffTrees(repo *RepoRecord, baseTreeOID, headTreeOID string) []FileChange {
	base := s.FlatTree(repo, baseTreeOID)
	head := s.FlatTree(repo, headTreeOID)
	pathSet := map[string]bool{}
	for p := range base {
		pathSet[p] = true
	}
	for p := range head {
		pathSet[p] = true
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var changes []FileChange
	for _, path := range paths {
		oldOID, hasOld := base[path]
		newOID, hasNew := head[path]
		if hasOld && hasNew && oldOID == newOID {
			continue
		}
		oldText := ""
		if hasOld {
			if obj := repo.Objects[oldOID]; obj != nil {
				oldText = string(obj.Data)
			}
		}
		newText := ""
		if hasNew {
			if obj := repo.Objects[newOID]; obj != nil {
				newText = string(obj.Data)
			}
		}
		additions, deletions := LineDiff(oldText, newText)
		change := FileChange{
			Filename:  path,
			Additions: additions,
			Deletions: deletions,
			Changes:   additions + deletions,
		}
		switch {
		case !hasOld:
			change.Status = "added"
			v := newOID
			change.NewOID = &v
		case !hasNew:
			change.Status = "removed"
			v := oldOID
			change.OldOID = &v
		default:
			change.Status = "modified"
			o, n := oldOID, newOID
			change.OldOID, change.NewOID = &o, &n
		}
		changes = append(changes, change)
	}
	return changes
}

// resolveAncestor turns the optional ancestor argument of MergeConflicts
// and MergedFlat into an oid, or "" when there is no merge base. No
// argument means "work it out here" (one merge-base walk); an explicit ""
// means "no ancestor".
func (s *Store) resolveAncestor(repo *RepoRecord, baseSHA, headSHA string, ancestor []string) string {
	if len(ancestor) == 0 {
		return gitobj.MergeBase(baseSHA, headSHA, s.Lookup(repo))
	}
	return ancestor[0]
}

// ancestorFlat returns the flat tree of the merge-base commit, or an
// empty map when there is no merge base.
func (s *Store) ancestorFlat(repo *RepoRecord, ancestorOID string) map[string]string {
	if ancestorOID == "" {
		return map[string]string{}
	}
	treeOID := ""
	if obj := repo.Objects[ancestorOID]; obj != nil {
		treeOID = gitobj.ParseCommit(obj.Data).Tree
	}
	return s.FlatTree(repo, treeOID)
}

// MergeConflicts reports whether merging headSHA into baseSHA conflicts
// (three-way, file granularity). An already-known merge base may be
// passed to skip the graph walk; pass "" for "no ancestor".
func (s *Store) MergeConflicts(repo *RepoRecord, baseSHA, headSHA string, ancestor ...string) bool {
	baseCommit := repo.Objects[baseSHA]
	headCommit := repo.Objects[headSHA]
	if baseCommit == nil || headCommit == nil || baseCommit.Type != gitobj.Commit || headCommit.Type != gitobj.Commit {
		return true
	}
	ancestorOID := s.resolveAncestor(repo, baseSHA, headSHA, ancestor)
	baseFlat := s.FlatTree(repo, gitobj.ParseCommit(baseCommit.Data).Tree)
	headFlat := s.FlatTree(repo, gitobj.ParseCommit(headCommit.Data).Tree)
	ancestorFlat := s.ancestorFlat(repo, ancestorOID)
	pathSet := map[string]bool{}
	for p := range baseFlat {
		pathSet[p] = true
	}
	for p := range headFlat {
		pathSet[p] = true
	}
	for p := range ancestorFlat {
		pathSet[p] = true
	}
	for path := range pathSet {
		a, hasA := ancestorFlat[path]
		b, hasB := baseFlat[path]
		h, hasH := headFlat[path]
		// A path counts as changed on a side when its oid differs from the
		// ancestor's (absent counts as a distinct value).
		baseChanged := hasB != hasA || (hasB && hasA && b != a)
		headChanged := hasH != hasA || (hasH && hasA && h != a)
		if baseChanged && headChanged && (hasB != hasH || (hasB && hasH && b != h)) {
			return true
		}
	}
	return false
}

// MergedFlat returns the merged flat tree for a squash merge of headSHA
// onto baseSHA, or nil when it conflicts. It takes an already-known
// merge base like MergeConflicts does, so one merge costs one graph walk,
// not three.
func (s *Store) MergedFlat(repo *RepoRecord, baseSHA, headSHA string, ancestor ...string) map[string]string {
	ancestorOID := s.resolveAncestor(repo, baseSHA, headSHA, ancestor)
	if s.MergeConflicts(repo, baseSHA, headSHA, ancestorOID) {
		return nil
	}
	baseCommit := repo.Objects[baseSHA]
	headCommit := repo.Objects[headSHA]
	if baseCommit == nil || headCommit == nil {
		return nil
	}
	merged := make(map[string]string)
	for k, v := range s.FlatTree(repo, gitobj.ParseCommit(baseCommit.Data).Tree) {
		merged[k] = v
	}
	headFlat := s.FlatTree(repo, gitobj.ParseCommit(headCommit.Data).Tree)
	ancestorFlat := s.ancestorFlat(repo, ancestorOID)
	for path, h := range headFlat {
		a, hasA := ancestorFlat[path]
		if !hasA || h != a {
			merged[path] = h
		}
	}
	for path, a := range ancestorFlat {
		if _, inHead := headFlat[path]; !inHead {
			if cur, ok := merged[path]; ok && cur == a {
				delete(merged, path)
			}
		}
	}
	return merged
}

// CreateLabel creates a label, or returns the existing one when the name
// is already taken.
func (s *Store) CreateLabel(repo *RepoRecord, name, color string) *LabelRecord {
	if existing, ok := repo.Labels[name]; ok {
		return existing
	}
	if color == "" {
		color = "ededed"
	}
	label := &LabelRecord{Name: name, Color: color, Description: ""}
	repo.Labels[name] = label
	return label
}

// CreateIssue creates an open issue. When a named label does not exist it
// returns nil and an error string naming the label.
func (s *Store) CreateIssue(repo *RepoRecord, title, body string, labels []string) (*IssueRecord, string) {
	var checked []string
	for _, name := range labels {
		if _, ok := repo.Labels[name]; !ok {
			return nil, fmt.Sprintf("Label '%s' does not exist", name)
		}
		checked = append(checked, name)
	}
	s.ensureInit()
	issue := &IssueRecord{
		ID:        s.nextObjID,
		Number:    repo.NextNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    checked,
		User:      Viewer.Login,
		CreatedAt: util.TrialNowISO(),
		UpdatedAt: util.TrialNowISO(),
	}
	s.nextObjID++
	repo.NextNumber++
	repo.Issues = append(repo.Issues, issue)
	repo.IssuesByNumber[issue.Number] = issue
	return issue, ""
}

// GetIssue returns an issue by number, or nil.
func (s *Store) GetIssue(repo *RepoRecord, number int) *IssueRecord {
	return repo.IssuesByNumber[number]
}

// CreatePull opens a pull request. It resolves the head and base refs,
// rejects a duplicate open pull for the same head and base, and returns
// nil plus an error string on any failure.
func (s *Store) CreatePull(repo *RepoRecord, title, body, head, base string) (*PullRecord, string) {
	headSHA := s.ResolveRef(repo, head)
	if headSHA == "" {
		return nil, fmt.Sprintf("Head branch '%s' does not exist", head)
	}
	baseSHA := s.ResolveRef(repo, base)
	if baseSHA == "" {
		return nil, fmt.Sprintf("Base branch '%s' does not exist", base)
	}
	for _, pull := range repo.Pulls {
		if pull.State == "open" && pull.HeadRef == head && pull.BaseRef == base {
			return nil, "A pull request already exists for this head and base"
		}
	}
	s.ensureInit()
	pull := &PullRecord{
		ID:        s.nextObjID,
		Number:    repo.NextNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		HeadRef:   head,
		HeadSha:   headSHA,
		BaseRef:   base,
		BaseSha:   baseSHA,
		User:      Viewer.Login,
		CreatedAt: util.TrialNowISO(),
		UpdatedAt: util.TrialNowISO(),
	}
	s.nextObjID++
	repo.NextNumber++
	repo.Pulls = append(repo.Pulls, pull)
	repo.PullsByNumber[pull.Number] = pull
	return pull, ""
}

// GetPull returns a pull request by number, or nil.
func (s *Store) GetPull(repo *RepoRecord, number int) *PullRecord {
	return repo.PullsByNumber[number]
}

// MergePull squash-merges a pull request onto its base branch. On success
// it returns the merge commit sha with an empty error and status 0; on
// failure it returns "" plus an error string and the HTTP status the
// caller should answer with (405 for a closed pull or a conflict, 422
// when the base branch is gone).
func (s *Store) MergePull(repo *RepoRecord, pull *PullRecord) (sha string, errMsg string, status int) {
	if pull.State != "open" {
		return "", "Pull request is not open", 405
	}
	baseRefName := "refs/heads/" + pull.BaseRef
	baseTip, ok := repo.Refs[baseRefName]
	if !ok {
		return "", "Base branch does not exist", 422
	}
	headTip, ok := repo.Refs["refs/heads/"+pull.HeadRef]
	if !ok {
		headTip = pull.HeadSha
	}
	// One graph walk for the whole merge: the base is found here and
	// handed down, instead of MergeConflicts and MergedFlat each walking
	// it again.
	ancestor := gitobj.MergeBase(baseTip, headTip, s.Lookup(repo))
	if s.MergeConflicts(repo, baseTip, headTip, ancestor) {
		return "", "Merge conflict", 405
	}
	merged := s.MergedFlat(repo, baseTip, headTip, ancestor)
	if merged == nil {
		return "", "Merge conflict", 405
	}
	treeOID := s.BuildTreeFromFlat(repo, merged)
	ident := ForgeIdent()
	message := fmt.Sprintf("Merge pull request #%d from %s\n\n%s", pull.Number, pull.HeadRef, pull.Title)
	mergeSHA := s.PutObject(repo, gitobj.Commit, gitobj.BuildCommit(treeOID, []string{baseTip}, ident, ident, message))
	repo.Refs[baseRefName] = mergeSHA
	pull.State = "closed"
	pull.Merged = true
	now := util.TrialNowISO()
	pull.MergedAt = &now
	pull.MergeCommitSha = &mergeSHA
	pull.UpdatedAt = now
	return mergeSHA, "", 0
}

// CreateComment adds a comment to an issue and clears the issue's cached
// view, so the next read rebuilds it.
func (s *Store) CreateComment(repo *RepoRecord, issue *IssueRecord, body string) *CommentRecord {
	s.ensureInit()
	comment := &CommentRecord{
		ID:        s.nextComment,
		Body:      body,
		User:      Viewer.Login,
		CreatedAt: util.TrialNowISO(),
	}
	s.nextComment++
	issue.Comments = append(issue.Comments, comment)
	issue.ViewBuf = nil
	return comment
}

// Reachable returns all objects reachable from a set of roots.
func (s *Store) Reachable(repo *RepoRecord, roots []string) map[string]*gitobj.GitObject {
	return gitobj.CollectReachable(roots, s.Lookup(repo))
}

// IsAncestor reports whether ancestor is reachable from tip.
func (s *Store) IsAncestor(repo *RepoRecord, ancestor, tip string) bool {
	return gitobj.IsAncestor(ancestor, tip, s.Lookup(repo))
}
