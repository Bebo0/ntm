package tracker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git unavailable: %v\n%s", err, out)
	}
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "config", "user.name", "Test")
	writeFile(t, dir, "tracked.txt", "original")
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "init")
	return dir
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// newTestRecorder samples repo and keys the ledger by that same directory, which
// is the shared-checkout shape; the worktree shape is covered by
// TestTwoAgentWorktreesProduceDetectableConflict.
func newTestRecorder(repo, identity string, store *FileChangeStore) *GitRecorder {
	return NewGitRecorder(GitRecorderConfig{
		Root:       repo,
		ProjectDir: repo,
		Session:    "sess",
		Identity:   identity,
		Store:      store,
	})
}

func sample(t *testing.T, r *GitRecorder) int {
	t.Helper()
	n, err := r.Sample(t.Context())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	return n
}

// A tree that was already dirty when monitoring started did not change now.
func TestGitRecorderFirstSampleOnlyBaselines(t *testing.T) {
	repo := newGitRepo(t)
	writeFile(t, repo, "tracked.txt", "edited before monitoring")

	store := NewFileChangeStore(50)
	r := newTestRecorder(repo, "cc-1", store)

	if n := sample(t, r); n != 0 {
		t.Errorf("first sample recorded %d changes, want 0 (baseline only)", n)
	}
	if got := len(store.All()); got != 0 {
		t.Errorf("store has %d entries after baseline, want 0", got)
	}
}

func TestGitRecorderRecordsNewAndModifiedFiles(t *testing.T) {
	repo := newGitRepo(t)
	store := NewFileChangeStore(50)
	r := newTestRecorder(repo, "cc-1", store)
	sample(t, r)

	writeFile(t, repo, "brand_new.txt", "new content")
	writeFile(t, repo, "tracked.txt", "modified content")

	if n := sample(t, r); n != 2 {
		t.Fatalf("recorded %d changes, want 2", n)
	}

	byPath := map[string]FileChange{}
	for _, entry := range store.All() {
		byPath[entry.Change.Path] = entry.Change
		if len(entry.Agents) != 1 || entry.Agents[0] != "cc-1" {
			t.Errorf("entry for %s attributed to %v, want [cc-1]", entry.Change.Path, entry.Agents)
		}
		if entry.Session != "sess" {
			t.Errorf("entry session = %q, want %q", entry.Session, "sess")
		}
	}
	if got := byPath["brand_new.txt"].Type; got != FileAdded {
		t.Errorf("brand_new.txt type = %q, want %q", got, FileAdded)
	}
	if got := byPath["tracked.txt"].Type; got != FileModified {
		t.Errorf("tracked.txt type = %q, want %q", got, FileModified)
	}
}

// A file that stays dirty without being touched again must not re-record on
// every tick, or a single edit would look like continuous churn.
func TestGitRecorderIgnoresUnchangedDirtyFile(t *testing.T) {
	repo := newGitRepo(t)
	store := NewFileChangeStore(50)
	r := newTestRecorder(repo, "cc-1", store)
	sample(t, r)

	writeFile(t, repo, "tracked.txt", "edited once")
	if n := sample(t, r); n != 1 {
		t.Fatalf("first edit recorded %d changes, want 1", n)
	}
	if n := sample(t, r); n != 0 {
		t.Errorf("unchanged dirty file recorded %d changes, want 0", n)
	}
}

func TestGitRecorderRecordsRepeatEdits(t *testing.T) {
	repo := newGitRepo(t)
	store := NewFileChangeStore(50)
	r := newTestRecorder(repo, "cc-1", store)
	sample(t, r)

	writeFile(t, repo, "tracked.txt", "first edit")
	sample(t, r)
	writeFile(t, repo, "tracked.txt", "second edit, a different length entirely")
	if n := sample(t, r); n != 1 {
		t.Errorf("second edit recorded %d changes, want 1", n)
	}
}

// Committing is not deleting. The delta walks paths that left `git status`, so
// without a stat against the tree root every commit would look like a deletion.
func TestGitRecorderCommitIsNotADeletion(t *testing.T) {
	repo := newGitRepo(t)
	store := NewFileChangeStore(50)
	r := newTestRecorder(repo, "cc-1", store)
	sample(t, r)

	writeFile(t, repo, "tracked.txt", "edited then committed")
	sample(t, r)

	gitCmd(t, repo, "add", ".")
	gitCmd(t, repo, "commit", "-m", "commit the edit")
	sample(t, r)

	for _, entry := range store.All() {
		if entry.Change.Type == FileDeleted {
			t.Errorf("committing %s was recorded as a deletion", entry.Change.Path)
		}
	}
}

func TestGitRecorderRecordsDeletion(t *testing.T) {
	repo := newGitRepo(t)
	store := NewFileChangeStore(50)
	r := newTestRecorder(repo, "cc-1", store)
	sample(t, r)

	if err := os.Remove(filepath.Join(repo, "tracked.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	sample(t, r)

	var found bool
	for _, entry := range store.All() {
		if entry.Change.Path == "tracked.txt" && entry.Change.Type == FileDeleted {
			found = true
		}
	}
	if !found {
		t.Error("deleting a tracked file was not recorded as a deletion")
	}
}

func TestGitRecorderHonorsGitignore(t *testing.T) {
	repo := newGitRepo(t)
	writeFile(t, repo, ".gitignore", "noise/\n")
	gitCmd(t, repo, "add", ".gitignore")
	gitCmd(t, repo, "commit", "-m", "ignore noise")

	store := NewFileChangeStore(50)
	r := newTestRecorder(repo, "cc-1", store)
	sample(t, r)

	writeFile(t, repo, "noise/generated.bin", "build output")
	if n := sample(t, r); n != 0 {
		t.Errorf("ignored files recorded %d changes, want 0", n)
	}
}

// Repo-relative paths are what let the same file compare equal across two
// agents' worktrees; absolute paths would never match and no conflict could
// ever be detected.
func TestGitRecorderPathsAreRepoRelative(t *testing.T) {
	repo := newGitRepo(t)
	store := NewFileChangeStore(50)
	r := newTestRecorder(repo, "cc-1", store)
	sample(t, r)

	writeFile(t, repo, "internal/pkg/file.go", "package pkg")
	sample(t, r)

	entries := store.All()
	if len(entries) != 1 {
		t.Fatalf("recorded %d entries, want 1", len(entries))
	}
	if got := entries[0].Change.Path; got != "internal/pkg/file.go" {
		t.Errorf("path = %q, want repo-relative %q", got, "internal/pkg/file.go")
	}
}

// The end-to-end feature: two agents editing the same repo-relative path in
// their own worktrees must surface as a conflict naming both agents.
func TestTwoAgentWorktreesProduceDetectableConflict(t *testing.T) {
	repoA := newGitRepo(t)
	repoB := newGitRepo(t)

	store := NewFileChangeStore(50)
	agentA := newTestRecorder(repoA, "cc-1", store)
	agentB := newTestRecorder(repoB, "cod-2", store)
	sample(t, agentA)
	sample(t, agentB)

	writeFile(t, repoA, "internal/serve/server.go", "edited by cc-1")
	writeFile(t, repoB, "internal/serve/server.go", "edited by cod-2, different length")
	sample(t, agentA)
	sample(t, agentB)

	conflicts := DetectConflicts(store.All())
	if len(conflicts) != 1 {
		t.Fatalf("detected %d conflicts, want 1: %+v", len(conflicts), conflicts)
	}
	c := conflicts[0]
	if c.Path != "internal/serve/server.go" {
		t.Errorf("conflict path = %q, want %q", c.Path, "internal/serve/server.go")
	}
	if len(c.Agents) != 2 || c.Agents[0] != "cc-1" || c.Agents[1] != "cod-2" {
		t.Errorf("conflict agents = %v, want [cc-1 cod-2]", c.Agents)
	}
}

// One agent editing one file repeatedly is not a conflict. This is what a
// shared-tree "attribute to every agent in the session" scheme would have
// reported as a critical three-way conflict.
func TestSingleAgentRepeatEditsAreNotAConflict(t *testing.T) {
	repo := newGitRepo(t)
	store := NewFileChangeStore(50)
	r := newTestRecorder(repo, "cc-1", store)
	sample(t, r)

	writeFile(t, repo, "tracked.txt", "edit one")
	sample(t, r)
	writeFile(t, repo, "tracked.txt", "edit two, longer than the first")
	sample(t, r)

	if conflicts := DetectConflicts(store.All()); len(conflicts) != 0 {
		t.Errorf("one agent's repeat edits reported %d conflicts, want 0: %+v", len(conflicts), conflicts)
	}
}

func TestGitRecorderNonRepoIsAnErrorNotAPanic(t *testing.T) {
	dir := t.TempDir()
	r := newTestRecorder(dir, "cc-1", NewFileChangeStore(10))
	if _, err := r.Sample(t.Context()); err == nil {
		t.Error("sampling a non-repository should report an error")
	}
}

func TestFileChangeStoreAddWrapsAtLimit(t *testing.T) {
	store := NewFileChangeStore(3)
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		store.Add(RecordedFileChange{
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Change:    FileChange{Path: name, Type: FileModified},
		})
	}

	all := store.All()
	if len(all) != 3 {
		t.Fatalf("store kept %d entries, want 3", len(all))
	}
	want := []string{"c", "d", "e"}
	for i, entry := range all {
		if entry.Change.Path != want[i] {
			t.Errorf("entry %d = %q, want %q (oldest-first after wrap)", i, entry.Change.Path, want[i])
		}
	}
}

// A failed ledger write must not advance the baseline. AppendFileChanges is one
// transaction, so a failure wrote nothing; advancing would compare the next
// sample against a state whose changes were never recorded anywhere and drop
// them for good on a transient busy database.
func TestSampleRetriesAfterAFailedLedgerWrite(t *testing.T) {
	fake := installFakeBackend(t)
	repo := newGitRepo(t)
	store := NewFileChangeStore(50)
	r := newTestRecorder(repo, "cc-1", store)
	sample(t, r)

	writeFile(t, repo, "tracked.txt", "an edit that fails to persist")

	fake.appendErr = errFakeBackend
	if _, err := r.Sample(t.Context()); err == nil {
		t.Fatal("expected the failing ledger write to be reported")
	}
	if got := len(store.All()); got != 0 {
		t.Errorf("in-memory ring holds %d entries after a failed write, want 0", got)
	}

	// The ledger recovers; the change must still be found rather than lost.
	fake.appendErr = nil
	n, err := r.Sample(t.Context())
	if err != nil {
		t.Fatalf("Sample after recovery: %v", err)
	}
	if n != 1 {
		t.Fatalf("recorded %d changes after recovery, want 1 (the edit was dropped)", n)
	}
	if len(fake.rows) != 1 || fake.rows[0].Path != "tracked.txt" {
		t.Errorf("ledger rows = %+v, want the retried tracked.txt change", fake.rows)
	}
}

// A manifest's project dir is not guaranteed to be the repository root. The
// reader keys the ledger by its git toplevel, so a recorder pointed at a
// subdirectory must resolve to the same key or every surface reports nothing
// while recording works perfectly.
func TestRecorderKeysLedgerByRepositoryRoot(t *testing.T) {
	fake := installFakeBackend(t)
	repo := newGitRepo(t)

	sub := filepath.Join(repo, "services", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	r := NewGitRecorder(GitRecorderConfig{
		Root:       repo,
		ProjectDir: sub, // a subdirectory, as a manifest may well carry
		Session:    "sess",
		Identity:   "cc-1",
		Store:      NewFileChangeStore(50),
	})
	sample(t, r)
	writeFile(t, repo, "tracked.txt", "an edit")
	sample(t, r)

	if len(fake.rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(fake.rows))
	}
	want := NormalizeProjectDir(repo)
	if fake.rows[0].ProjectDir != want {
		t.Errorf("ledger key = %q, want the repository root %q", fake.rows[0].ProjectDir, want)
	}

	// And a reader standing at the root finds it.
	chdir(t, repo)
	original := GlobalFileChanges
	GlobalFileChanges = NewFileChangeStore(500)
	t.Cleanup(func() { GlobalFileChanges = original })
	if got := RecordedChangesSince(time.Now().Add(-time.Hour)); len(got) != 1 {
		t.Errorf("reader at the repository root found %d changes, want 1", len(got))
	}
}

// git reports paths relative to the repository root even when it runs in a
// subdirectory. A recorder rooted in a manifest's subdirectory used to stat
// "services/tracked.txt" for "tracked.txt": every signature came back empty, so
// a repeat edit was missed, and the stat after a commit failed, so the commit
// was recorded as a deletion.
func TestGitRecorderRootedInASubdirectory(t *testing.T) {
	installFakeBackend(t)
	repo := newGitRepo(t)
	sub := filepath.Join(repo, "services")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	store := NewFileChangeStore(50)
	r := NewGitRecorder(GitRecorderConfig{Root: sub, ProjectDir: sub, Session: "sess", Identity: "cc-1", Store: store})
	sample(t, r)

	writeFile(t, repo, "tracked.txt", "edit one")
	if n := sample(t, r); n != 1 {
		t.Fatalf("first edit recorded %d changes, want 1", n)
	}
	writeFile(t, repo, "tracked.txt", "edit two, a different length entirely")
	if n := sample(t, r); n != 1 {
		t.Errorf("repeat edit recorded %d changes, want 1", n)
	}

	gitCmd(t, repo, "add", ".")
	gitCmd(t, repo, "commit", "-m", "commit the edits")
	sample(t, r)
	for _, entry := range store.All() {
		if entry.Change.Type == FileDeleted {
			t.Errorf("committing %s was recorded as a deletion", entry.Change.Path)
		}
	}
}

func fillDir(t *testing.T, dir string, n int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for i := range n {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d", i)), nil, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

// GH #337: an untracked directory of agent scratch output made every sample
// list, parse and stat each file in it. Past the listing limit it is held as
// one entry: recorded once when it appears, and silent while it keeps filling.
func TestGitRecorderHoldsLargeUntrackedDirectoryWhole(t *testing.T) {
	installFakeBackend(t)
	repo := newGitRepo(t)
	store := NewFileChangeStore(50)
	r := newTestRecorder(repo, "cc-1", store)
	sample(t, r)

	fillDir(t, filepath.Join(repo, "scratch"), untrackedDirListLimit+1)
	writeFile(t, repo, "tracked.txt", "a real edit alongside the scratch output")
	if n := sample(t, r); n != 2 {
		t.Fatalf("recorded %d changes, want 2 (scratch/ and tracked.txt)", n)
	}
	byPath := map[string]FileChangeType{}
	for _, entry := range store.All() {
		byPath[entry.Change.Path] = entry.Change.Type
	}
	if byPath["scratch/"] != FileAdded || byPath["tracked.txt"] != FileModified {
		t.Errorf("recorded %v, want scratch/ added and tracked.txt modified", byPath)
	}

	writeFile(t, repo, "scratch/more-output.txt", "keeps filling")
	if n := sample(t, r); n != 0 {
		t.Errorf("filling a directory held whole recorded %d changes, want 0", n)
	}

	snapshot, err := gitDirtySnapshot(t.Context(), repo)
	if err != nil {
		t.Fatalf("gitDirtySnapshot: %v", err)
	}
	if len(snapshot) != 2 {
		t.Errorf("snapshot holds %d entries, want 2 (scratch/ and tracked.txt)", len(snapshot))
	}
}

// A directory that shrinks back under the limit becomes listable again. Its
// files were inside it all along, so listing them is not an addition per file.
func TestGitRecorderUntrackedDirectoryShrinkingBackIsNotAChange(t *testing.T) {
	installFakeBackend(t)
	repo := newGitRepo(t)
	scratch := filepath.Join(repo, "scratch")
	fillDir(t, scratch, untrackedDirListLimit+1)

	store := NewFileChangeStore(50)
	r := newTestRecorder(repo, "cc-1", store)
	sample(t, r)

	for i := 2; i <= untrackedDirListLimit; i++ {
		if err := os.Remove(filepath.Join(scratch, fmt.Sprintf("f%05d", i))); err != nil {
			t.Fatalf("remove: %v", err)
		}
	}
	if n := sample(t, r); n != 0 {
		t.Fatalf("listing a shrunken directory recorded %d changes, want 0", n)
	}

	// Listed file by file again, its files carry signatures once more.
	writeFile(t, repo, "scratch/f00000", "now edited")
	if n := sample(t, r); n != 1 {
		t.Errorf("editing a file in the listed directory recorded %d changes, want 1", n)
	}
}

// A directory name handed back to git to be listed is a path, not a pathspec.
// Parsed as pathspec magic, a directory named ":!x" means "everything except
// x/" and would list the very directory held whole for being too large.
func TestUntrackedListingUsesLiteralPathspecs(t *testing.T) {
	repo := newGitRepo(t)
	writeFile(t, repo, ":!x/small.txt", "small")
	fillDir(t, filepath.Join(repo, "big"), untrackedDirListLimit+1)

	snapshot, err := gitDirtySnapshot(t.Context(), repo)
	if err != nil {
		t.Fatalf("gitDirtySnapshot: %v", err)
	}
	if _, ok := snapshot[":!x/small.txt"]; !ok {
		t.Errorf("small directory :!x/ was not listed file by file")
	}
	if _, ok := snapshot["big/"]; !ok || len(snapshot) != 2 {
		t.Errorf("snapshot = %d entries, want big/ held whole plus :!x/small.txt", len(snapshot))
	}
}

func TestListableUntrackedDirsBoundsEachDirectoryAndTheSample(t *testing.T) {
	top := t.TempDir()
	fillDir(t, filepath.Join(top, "a"), 3)
	fillDir(t, filepath.Join(top, "b"), 5)
	fillDir(t, filepath.Join(top, "c", "nested"), 1) // two entries: nested/ and its file
	fillDir(t, filepath.Join(top, "d"), 2)

	// b passes the per-directory limit of 4 while the budget is still whole;
	// a fits (3 of 5 spent); c fits the remaining 2; nothing is left for d.
	got := listableUntrackedDirs(top, []string{"b/", "a/", "c/", "d/"}, 4, 5)
	want := []string{"a/", "c/"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("listable = %v, want %v", got, want)
	}
}

func TestCountEntriesWithinStopsPastTheLimit(t *testing.T) {
	dir := t.TempDir()
	fillDir(t, dir, 600)

	if n, within := countEntriesWithin(dir, 600); !within || n != 600 {
		t.Errorf("countEntriesWithin(600 entries, 600) = %d, %v; want 600, true", n, within)
	}
	if n, within := countEntriesWithin(dir, 10); within || n != 11 {
		t.Errorf("countEntriesWithin(600 entries, 10) = %d, %v; want to stop at 11, false", n, within)
	}
	if n, within := countEntriesWithin(filepath.Join(dir, "missing"), 10); !within || n != 0 {
		t.Errorf("a vanished directory counted %d, %v; want 0, true", n, within)
	}
}
