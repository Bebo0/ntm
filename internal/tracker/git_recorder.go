package tracker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Git-backed file change capture.
//
// The original capture pipeline (SnapshotDirectory + RecordFileChanges) walked
// the entire project tree twice per dispatch and slept in a goroutine to do it;
// it was deleted in 31fc0f5f, which left every reader of GlobalFileChanges —
// `ntm changes`, `ntm conflicts`, --robot-status, the dashboard Files panel and
// the work-coordination adapter — permanently empty while still reporting a
// clean result.
//
// This replaces it with sampling that costs a `git status` plus one stat per
// dirty file: git narrows the candidate set (honoring .gitignore) and the stats
// distinguish a file edited twice from one that merely stayed dirty.

// gitStatusTimeout bounds the git invocations of one sample. Sampling is
// best-effort telemetry and must never wedge the monitor that drives it.
const gitStatusTimeout = 20 * time.Second

// Untracked directories are where "one stat per dirty file" stops being cheap:
// a single directory of agent scratch output can hold tens of thousands of
// files, and listing it with --untracked-files=all made an idle session's
// monitor walk, parse and stat every one of them on every tick (GH #337).
// The contents of a wholly untracked directory are listed file by file only
// while they stay small; a directory past these bounds is held as the one entry
// git's default untracked mode gives it. (That mode still lists loose untracked
// files one by one when they sit in a directory that also holds tracked files;
// these bounds cover wholly untracked directories, where scratch output goes.)
const (
	// untrackedDirListLimit is the most filesystem entries one untracked
	// directory may hold and still be listed file by file.
	untrackedDirListLimit = 1000
	// untrackedListBudget bounds the filesystem entries the size probes read
	// in one sample, listed or not, so neither many small directories nor many
	// large ones can add back up to the cost a single large one is kept from
	// having. A directory the budget does not reach is held whole.
	untrackedListBudget = 5000
	// untrackedPathspecBatch bounds the directories handed to one git
	// invocation, keeping the argument list well inside every platform's
	// ARG_MAX however long the directory names are.
	untrackedPathspecBatch = 500
)

// GitRecorder observes one git working tree and records what changed since its
// previous observation, attributed to a single identity.
//
// Attribution is only ever as precise as the tree allows, and is never invented:
// a per-agent worktree yields that agent's name, while a shared checkout yields
// the session, because git cannot say which of several agents in one working
// tree wrote a file. That distinction matters — DetectConflicts treats three
// distinct agents on one path as a critical conflict, so attributing a shared
// tree's edits to every agent in the session would manufacture critical
// conflicts out of one agent editing one file twice.
type GitRecorder struct {
	root       string
	projectDir string
	session    string
	identity   string
	store      *FileChangeStore

	// top is the working tree root that contains root, resolved on the first
	// sample. git reports every path relative to it — not to root, which a
	// manifest may set to a subdirectory — so it is the base for every stat.
	top string

	prev    map[string]FileState
	sampled bool
}

// GitRecorderConfig describes one attribution unit.
type GitRecorderConfig struct {
	// Root is the git working tree to sample. For an isolated agent this is
	// that agent's worktree, not the project checkout.
	Root string
	// ProjectDir keys the durable ledger. It is the project the changes belong
	// to rather than the sampled tree, so every agent's worktree reports into
	// the one project a reader will later ask about. Defaults to Root.
	ProjectDir string
	// Session and Identity are the attribution recorded with each change.
	Session  string
	Identity string
	// Store is the in-process ring to also write to. Defaults to
	// GlobalFileChanges.
	Store *FileChangeStore
}

// NewGitRecorder returns a recorder for the working tree described by cfg.
func NewGitRecorder(cfg GitRecorderConfig) *GitRecorder {
	store := cfg.Store
	if store == nil {
		store = GlobalFileChanges
	}
	projectDir := cfg.ProjectDir
	if projectDir == "" {
		projectDir = cfg.Root
	}
	return &GitRecorder{
		root: cfg.Root,
		// Resolved the same way a reader resolves its own location, not merely
		// normalized. A reader keys the ledger by its git toplevel, so a
		// manifest whose project dir is a subdirectory of the repository would
		// be written under one key and read under another, and every surface
		// would show nothing while recording worked perfectly.
		projectDir: resolveProjectDir(projectDir),
		session:    cfg.Session,
		identity:   cfg.Identity,
		store:      store,
	}
}

// Sample takes one observation and records the delta since the previous one,
// returning how many changes were recorded.
//
// The first call only establishes a baseline: a tree that was already dirty when
// monitoring started did not change *now*, and recording it as though it had
// would date every pre-existing edit to monitor startup.
func (r *GitRecorder) Sample(ctx context.Context) (int, error) {
	if r == nil || r.root == "" {
		return 0, nil
	}

	if r.top == "" {
		top, err := gitToplevel(ctx, r.root)
		if err != nil {
			return 0, err
		}
		r.top = top
	}

	current, err := gitDirtySnapshot(ctx, r.top)
	if err != nil {
		return 0, err
	}

	if !r.sampled {
		r.prev = current
		r.sampled = true
		return 0, nil
	}

	changes := DetectFileChanges(r.top, r.prev, current)
	if len(changes) == 0 {
		r.prev = current
		return 0, nil
	}

	now := time.Now()
	entries := make([]RecordedFileChange, 0, len(changes))
	for _, change := range changes {
		entries = append(entries, RecordedFileChange{
			Timestamp: now,
			Session:   r.session,
			Agents:    []string{r.identity},
			Change:    change,
		})
	}

	// Persist before advancing. The readers all live in other processes, so the
	// durable ledger is what actually reaches `ntm changes`, `ntm conflicts` and
	// the dashboard, and AppendFileChanges is one transaction: a failure wrote
	// nothing. Advancing prev first would compare the next sample against a
	// state whose changes were never recorded anywhere, dropping them for good
	// on a transient busy database. Leaving prev where it is costs a re-detect
	// on the next tick instead.
	if err := persistChanges(r.projectDir, entries); err != nil {
		return 0, err
	}

	r.prev = current
	for _, entry := range entries {
		r.store.Add(entry)
	}
	return len(entries), nil
}

// DetectFileChanges compares two dirty-file snapshots of the tree at root and
// returns the delta. Paths are repo-relative so the same file compares equal
// across two agents' worktrees, which is what makes cross-worktree conflict
// detection possible.
func DetectFileChanges(root string, before, after map[string]FileState) []FileChange {
	changes := make([]FileChange, 0)
	heldWhole := untrackedDirsHeldWhole(before)

	for path, afterState := range after {
		afterState := afterState
		beforeState, existed := before[path]

		// A file inside a directory the previous snapshot held as one entry
		// was not absent then, only unlisted: the directory has shrunk back
		// under the listing bounds. Nothing says the file itself changed, so
		// reporting it would invent one addition per file it holds.
		if !existed && insideAny(path, heldWhole) {
			continue
		}

		// A snapshot holds only dirty files, so a path absent from before was
		// clean then: git's own status says whether it appeared, was edited, or
		// was removed.
		if !existed {
			switch {
			case strings.Contains(afterState.GitStatus, "D"):
				changes = append(changes, FileChange{Path: path, Type: FileDeleted, After: &afterState})
			case strings.ContainsAny(afterState.GitStatus, "MR"):
				changes = append(changes, FileChange{Path: path, Type: FileModified, After: &afterState})
			default:
				changes = append(changes, FileChange{Path: path, Type: FileAdded, After: &afterState})
			}
			continue
		}

		if strings.Contains(afterState.GitStatus, "D") {
			if !strings.Contains(beforeState.GitStatus, "D") {
				beforeState := beforeState
				changes = append(changes, FileChange{Path: path, Type: FileDeleted, Before: &beforeState, After: &afterState})
			}
			continue
		}

		// Still dirty in both samples: only a changed signature means it was
		// edited again rather than merely staying dirty.
		if !afterState.ModTime.Equal(beforeState.ModTime) || afterState.Size != beforeState.Size {
			beforeState := beforeState
			changes = append(changes, FileChange{Path: path, Type: FileModified, Before: &beforeState, After: &afterState})
		}
	}

	for path, beforeState := range before {
		if _, ok := after[path]; ok {
			continue
		}
		if strings.Contains(beforeState.GitStatus, "D") {
			// Already reported as deleted when it went missing.
			continue
		}
		// Leaving `git status` usually means committed or reverted, not deleted.
		// Stat against the repository root — the paths here are repo-relative,
		// so statting them against anything else (the process working
		// directory, or a manifest's subdirectory) would call every committed
		// file a deletion.
		if _, err := os.Stat(filepath.Join(root, path)); err == nil {
			continue
		}
		beforeState := beforeState
		changes = append(changes, FileChange{Path: path, Type: FileDeleted, Before: &beforeState})
	}

	return changes
}

// gitDirtySnapshot returns every path git reports as dirty in the working tree
// rooted at top, keyed repo-relative, with the stat signature used to spot
// repeat edits.
//
// Untracked content is listed file by file only within untrackedDirListLimit
// and untrackedListBudget. Larger untracked directories stay single "dir/"
// entries, which carry no signature (see stateFor), so the cost of a sample is
// bounded by the tracked dirty files rather than by whatever accumulates in an
// untracked directory.
func gitDirtySnapshot(ctx context.Context, top string) (map[string]FileState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, gitStatusTimeout)
	defer cancel()

	// -z survives paths containing spaces, quotes and newlines, which the
	// default porcelain output escapes instead. The default untracked mode
	// reports a wholly untracked directory as one "dir/" entry without
	// listing what is inside it.
	out, err := runGit(ctx, top, "status", "--porcelain", "-z", "--untracked-files=normal")
	if err != nil {
		return nil, err
	}

	snapshot := make(map[string]FileState)
	var untrackedDirs []string
	for _, record := range parsePorcelainZ(out) {
		if isUntrackedDir(record.path, record.status) {
			untrackedDirs = append(untrackedDirs, record.path)
		}
		snapshot[record.path] = stateFor(top, record.path, record.status)
	}

	listed := listableUntrackedDirs(top, untrackedDirs, untrackedDirListLimit, untrackedListBudget)
	for len(listed) > 0 {
		batch := listed[:min(len(listed), untrackedPathspecBatch)]
		listed = listed[len(batch):]

		// Literal pathspecs: a directory name is a path, never a glob or
		// pathspec magic (":!x/" would otherwise mean "everything but x/").
		args := []string{"--literal-pathspecs", "status", "--porcelain", "-z", "--untracked-files=all", "--"}
		for _, dir := range batch {
			delete(snapshot, dir)
			args = append(args, dir)
		}
		out, err = runGit(ctx, top, args...)
		if err != nil {
			return nil, err
		}
		for _, record := range parsePorcelainZ(out) {
			snapshot[record.path] = stateFor(top, record.path, record.status)
		}
	}

	return snapshot, nil
}

// porcelainRecord is one entry of `git status --porcelain -z`.
type porcelainRecord struct {
	status string
	path   string
}

func parsePorcelainZ(out []byte) []porcelainRecord {
	var parsed []porcelainRecord
	records := bytes.Split(out, []byte{0})
	for i := 0; i < len(records); i++ {
		record := string(records[i])
		if len(record) < 4 {
			continue
		}
		status := strings.TrimSpace(record[:2])
		path := record[3:]

		// Renames and copies spend a second NUL-record on the source path.
		if strings.ContainsAny(record[:2], "RC") {
			i++
		}
		if path == "" {
			continue
		}
		parsed = append(parsed, porcelainRecord{status: status, path: path})
	}
	return parsed
}

// stateFor builds the snapshot entry for one reported path.
//
// A directory entry — an untracked directory held whole, or a nested
// repository — carries no signature. A directory's own mtime moves only when
// its direct children are added or removed, so it would report some of what
// happens inside the directory and miss the rest; an entry that says only "this
// directory is untracked" is the honest one. It is recorded when it appears and
// when it disappears.
func stateFor(top, path, status string) FileState {
	state := FileState{GitStatus: status}
	if strings.HasSuffix(path, "/") {
		return state
	}
	if info, err := os.Stat(filepath.Join(top, path)); err == nil {
		state.ModTime = info.ModTime()
		state.Size = info.Size()
	}
	return state
}

func isUntrackedDir(path, status string) bool {
	return status == "??" && strings.HasSuffix(path, "/")
}

// untrackedDirsHeldWhole returns the set of untracked directories a snapshot
// holds as single entries.
func untrackedDirsHeldWhole(snapshot map[string]FileState) map[string]bool {
	dirs := make(map[string]bool)
	for path, state := range snapshot {
		if isUntrackedDir(path, state.GitStatus) {
			dirs[path] = true
		}
	}
	return dirs
}

// insideAny reports whether path lies strictly inside one of dirs, each of
// which ends in "/". It looks up each ancestor of path rather than scanning
// dirs, so the cost follows path depth, not how many directories are held.
func insideAny(path string, dirs map[string]bool) bool {
	if len(dirs) == 0 {
		return false
	}
	for i := 0; i < len(path)-1; i++ {
		if path[i] == '/' && dirs[path[:i+1]] {
			return true
		}
	}
	return false
}

// listableUntrackedDirs returns, in their given order, the untracked
// directories small enough to list file by file: each holds at most dirLimit
// filesystem entries. budget bounds the entries the size probes read, listed
// or not; once it is spent, the remaining directories are held whole without
// being probed.
//
// Sizes are counted on the filesystem, so ignored files count too. That can
// only hold a directory whole sooner; it can never let an unbounded one be
// listed.
func listableUntrackedDirs(top string, dirs []string, dirLimit, budget int) []string {
	var listed []string
	for _, dir := range dirs {
		limit := min(dirLimit, budget)
		if limit <= 0 {
			break
		}
		n, within := countEntriesWithin(filepath.Join(top, dir), limit)
		budget -= n
		if within {
			listed = append(listed, dir)
		}
	}
	return listed
}

// countEntriesWithin counts the filesystem entries below dir and reports
// whether there are at most limit of them. It reads directories in batches and
// stops as soon as the limit is passed, so a directory of a hundred thousand
// files costs about one batch past the limit, not a hundred thousand reads.
// Symlinks are counted but never followed; git does not follow them either.
func countEntriesWithin(dir string, limit int) (int, bool) {
	count := 0
	pending := []string{dir}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]

		f, err := os.Open(current)
		if err != nil {
			// Unreadable or already gone: git cannot list it either.
			continue
		}
		for {
			entries, readErr := f.ReadDir(256)
			for _, entry := range entries {
				count++
				if count > limit {
					_ = f.Close()
					return count, false
				}
				if entry.IsDir() {
					pending = append(pending, filepath.Join(current, entry.Name()))
				}
			}
			if readErr != nil {
				break
			}
		}
		_ = f.Close()
	}
	return count, true
}

// gitToplevel returns the root of the working tree that contains dir.
func gitToplevel(ctx context.Context, dir string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, gitStatusTimeout)
	defer cancel()

	out, err := runGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return "", fmt.Errorf("git rev-parse --show-toplevel in %s: no working tree", dir)
	}
	return top, nil
}

// runGit runs one read-only git command in dir.
//
// --no-optional-locks stops `git status` from refreshing the index on the
// sampler's behalf. That refresh takes index.lock, the lock every agent in the
// tree needs for its own add or commit, and a background sampler running every
// few seconds must never be the reason one of those fails.
func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		// Name the subcommand, not every pathspec after it.
		name := "git"
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				name = "git " + arg
				break
			}
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s in %s: %w", name, dir, ctx.Err())
		}
		return nil, fmt.Errorf("%s in %s: %w", name, dir, err)
	}
	return out, nil
}
