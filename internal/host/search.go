package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/soren-labs/rmc/internal/protocol"
)

// SearchInput lists files under a directory or searches their contents. It
// is a read-only alternative to running find/grep/rg through exec_command:
// the same on every platform, bounded, and annotated read-only for clients.
type SearchInput struct {
	Path            string   `json:"path,omitempty"`
	Cwd             string   `json:"cwd,omitempty"`
	Pattern         string   `json:"pattern,omitempty"`
	Literal         bool     `json:"literal,omitempty"`
	CaseInsensitive bool     `json:"case_insensitive,omitempty"`
	Glob            string   `json:"glob,omitempty"`
	Exclude         []string `json:"exclude,omitempty"`
	IncludeHidden   bool     `json:"include_hidden,omitempty"`
	MaxDepth        *int     `json:"max_depth,omitempty"`
	MaxResults      *int     `json:"max_results,omitempty"`
	MaxBytes        *int     `json:"max_bytes,omitempty"`
	// ContextLines returns up to this many lines around each match.
	ContextLines *int `json:"context_lines,omitempty"`
	// IncludeDependencies descends the dependency and cache directories that
	// are skipped by default (see dependencyDir).
	IncludeDependencies bool `json:"include_dependencies,omitempty"`
}

const (
	searchFileLimit   = 16 << 20 // largest file whose content is searched
	searchMaxVisited  = 200000
	searchTimeBudget  = 20 * time.Second
	searchLineBytes   = 512
	searchBinaryProbe = 8192
	// searchSkippedListed bounds how many default-skipped directories are
	// named in the result; all of them are counted.
	searchSkippedListed = 20
)

// dependencyDirNames are directories of installed dependencies or generated
// caches that are rarely what a search is after and often hold more entries
// than the rest of a project together.
var dependencyDirNames = map[string]bool{"node_modules": true, "bower_components": true, "__pycache__": true}

// cacheDirSignature starts a CACHEDIR.TAG file (https://bford.info/cachedir/),
// which Cargo writes into target/ and many tools write into their caches.
const cacheDirSignature = "Signature: 8a477f597d28d172789f06886806bc55"

// dependencyDir reports whether a directory below the search root is skipped
// by default: a well-known dependency directory name, a Python virtual
// environment (pyvenv.cfg) or a tagged cache directory (CACHEDIR.TAG).
func dependencyDir(full, name string) bool {
	if dependencyDirNames[name] {
		return true
	}
	if info, err := os.Lstat(filepath.Join(full, "pyvenv.cfg")); err == nil && info.Mode().IsRegular() {
		return true
	}
	f, err := openRegular(filepath.Join(full, "CACHEDIR.TAG"), os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(cacheDirSignature))
	n, _ := io.ReadFull(f, head)
	return string(head[:n]) == cacheDirSignature
}

type searchStats struct {
	visitedFiles, visitedDirs, matchedFiles int
	binary, tooLarge, unreadable, symlinks  int
	dependencyDirs                          int
	dependencyPaths                         []string
}

var errSearchStop = errors.New("search limit reached")

func (r *Runtime) search(ctx context.Context, in SearchInput) (protocol.Result, error) {
	maxDepth, err := bounds(in.MaxDepth, 64, 1, 64, "max_depth")
	if err != nil {
		return protocol.Result{}, err
	}
	maxResults, err := bounds(in.MaxResults, 200, 1, 2000, "max_results")
	if err != nil {
		return protocol.Result{}, err
	}
	budget, err := bounds(in.MaxBytes, 65536, 1024, maxFileBytes, "max_bytes")
	if err != nil {
		return protocol.Result{}, err
	}
	contextLines, err := bounds(in.ContextLines, 0, 0, 10, "context_lines")
	if err != nil {
		return protocol.Result{}, err
	}
	if len(in.Pattern) > 4096 || len(in.Glob) > 1024 || len(in.Exclude) > 32 {
		return protocol.Result{}, errors.New("pattern is limited to 4096 bytes, glob to 1024 bytes and exclude to 32 entries")
	}
	var re *regexp.Regexp
	anchored := false
	if in.Pattern != "" {
		expr := in.Pattern
		if in.Literal {
			expr = regexp.QuoteMeta(expr)
		}
		if in.CaseInsensitive {
			expr = "(?i)" + expr
		}
		if re, err = regexp.Compile(expr); err != nil {
			return protocol.Result{}, fmt.Errorf("invalid pattern: %w", err)
		}
		anchored = hasAnchors(expr)
	}
	for _, g := range append([]string{in.Glob}, in.Exclude...) {
		if err := validGlob(g); err != nil {
			return protocol.Result{}, err
		}
	}
	root := in.Path
	if root == "" {
		root = "."
	}
	if root, err = r.path(root, in.Cwd); err != nil {
		return protocol.Result{}, err
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return protocol.Result{}, err
	}
	deadline := time.Now().Add(searchTimeBudget)
	if d, ok := ctx.Deadline(); ok && d.Add(-2*time.Second).Before(deadline) {
		deadline = d.Add(-2 * time.Second)
	}
	var st searchStats
	var results []map[string]any
	used, reason := 0, ""
	add := func(item map[string]any, cost int) error {
		if len(results) >= maxResults {
			reason = "max_results"
			return errSearchStop
		}
		if used+cost > budget {
			reason = "max_bytes"
			return errSearchStop
		}
		used += cost
		results = append(results, item)
		return nil
	}
	visit := func(full string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			reason = "time"
			return errSearchStop
		}
		if st.visitedFiles+st.visitedDirs >= searchMaxVisited {
			reason = "visited_entries"
			return errSearchStop
		}
		rel := relSlash(root, full)
		if walkErr != nil {
			st.unreadable++
			if d != nil && d.IsDir() && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		isDir := d.IsDir()
		if rel != "." {
			if !in.IncludeHidden && strings.HasPrefix(d.Name(), ".") || excluded(in.Exclude, rel) {
				if isDir {
					return filepath.SkipDir
				}
				return nil
			}
		} else if !isDir {
			// The search root itself is a file.
			rel = filepath.Base(full)
		}
		if isDir {
			st.visitedDirs++
			// The root itself is never skipped, so naming a dependency
			// directory as path searches it.
			skip := rel != "." && !in.IncludeDependencies && dependencyDir(full, d.Name())
			if re == nil && rel != "." && matchGlob(in.Glob, rel) {
				item := map[string]any{"path": rel, "type": "directory"}
				if skip {
					item["contents_skipped"] = true
				}
				if err := add(item, len(rel)+60); err != nil {
					return err
				}
			}
			if skip {
				st.dependencyDirs++
				if len(st.dependencyPaths) < searchSkippedListed {
					st.dependencyPaths = append(st.dependencyPaths, rel)
				}
				return filepath.SkipDir
			}
			// Entries below a directory at max_depth would be deeper.
			if rel != "." && depth(rel) >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		st.visitedFiles++
		if !matchGlob(in.Glob, rel) {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			st.symlinks++
			if re == nil {
				return add(map[string]any{"path": rel, "type": "symlink"}, len(rel)+40)
			}
			return nil
		}
		if re == nil {
			item := map[string]any{"path": rel, "type": "file"}
			if !d.Type().IsRegular() {
				item["type"] = "special"
			} else if info, err := d.Info(); err == nil {
				item["size"] = info.Size()
			}
			return add(item, len(rel)+60)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return searchFile(full, rel, re, anchored, contextLines, &st, add)
	}
	if rootInfo.IsDir() {
		err = filepath.WalkDir(root, visit)
	} else {
		err = visit(root, fs.FileInfoToDirEntry(rootInfo), nil)
	}
	if err != nil && !errors.Is(err, errSearchStop) && !errors.Is(err, filepath.SkipDir) {
		return protocol.Result{}, err
	}
	out := map[string]any{
		"root":          root,
		"files_visited": st.visitedFiles,
		"dirs_visited":  st.visitedDirs,
		"truncated":     reason != "",
		"skipped": map[string]int{
			"binary": st.binary, "too_large": st.tooLarge, "unreadable": st.unreadable, "symlinks": st.symlinks,
			"dependency_dirs": st.dependencyDirs,
		},
	}
	if st.dependencyDirs > 0 {
		// Name them so a caller knows where else to look.
		out["skipped_dependency_dirs"] = st.dependencyPaths
	}
	if reason != "" {
		out["truncated_reason"] = reason
	}
	if results == nil {
		results = []map[string]any{}
	}
	if re != nil {
		out["mode"] = "content"
		out["matches"] = results
		out["matched_files"] = st.matchedFiles
	} else {
		out["mode"] = "files"
		out["entries"] = results
	}
	return protocol.JSONResult(out), nil
}

// searchFile reports matching lines of one regular file. It opens without
// blocking (a path swapped for a FIFO cannot hang the call), skips binary and
// oversized files, and runs the expression over the whole file first so most
// non-matching files cost a single pass. That pass is skipped for anchored
// patterns: over the whole file, ^ and $ would match only at its start and
// end, not at each line. With contextLines, each match also
// carries the lines around it; lines already returned with the previous
// match are not repeated, and trailing context stops before the next match.
func searchFile(full, rel string, re *regexp.Regexp, anchored bool, contextLines int, st *searchStats, add func(map[string]any, int) error) error {
	f, err := openRegular(full, os.O_RDONLY, 0)
	if err != nil {
		st.unreadable++
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, searchFileLimit+1))
	if err != nil {
		st.unreadable++
		return nil
	}
	if len(data) > searchFileLimit {
		st.tooLarge++
		return nil
	}
	if bytes.IndexByte(data[:min(len(data), searchBinaryProbe)], 0) >= 0 {
		st.binary++
		return nil
	}
	if !anchored && !re.Match(data) {
		return nil
	}
	var lines [][]byte
	for rest := data; len(rest) > 0; {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		lines = append(lines, bytes.TrimSuffix(line, []byte("\r")))
	}
	var matches []int
	for i, line := range lines {
		if re.Match(line) {
			matches = append(matches, i)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	st.matchedFiles++
	returned := -1 // index of the last line already returned for this file
	for k, i := range matches {
		text, cut := searchLine(lines[i])
		item := map[string]any{"path": rel, "line": i + 1, "text": text}
		if cut {
			item["text_truncated"] = true
		}
		cost := len(rel) + len(text) + 40
		if contextLines > 0 {
			last := min(i+contextLines, len(lines)-1)
			if k+1 < len(matches) {
				last = min(last, matches[k+1]-1)
			}
			before, cutB, n := searchContext(lines[max(i-contextLines, returned+1):i])
			after, cutA, m := searchContext(lines[i+1 : last+1])
			item["before"], item["after"] = before, after
			if cutB || cutA {
				item["context_truncated"] = true
			}
			cost += n + m
			returned = last
		}
		if err := add(item, cost); err != nil {
			return err
		}
	}
	return nil
}

// searchLine returns a line as valid UTF-8 text of at most searchLineBytes.
func searchLine(line []byte) (string, bool) {
	cut := false
	if len(line) > searchLineBytes {
		line, cut = line[:completeUTF8Prefix(line[:searchLineBytes])], true
	}
	return strings.ToValidUTF8(string(line), string(utf8.RuneError)), cut
}

func searchContext(lines [][]byte) ([]string, bool, int) {
	out := make([]string, 0, len(lines))
	cut, cost := false, 0
	for _, l := range lines {
		text, c := searchLine(l)
		out = append(out, text)
		cut = cut || c
		cost += len(text) + 4
	}
	return out, cut, cost
}

// hasAnchors reports whether an expression uses ^, $, \A or \z, whose meaning
// differs between one line and a whole file.
func hasAnchors(expr string) bool {
	tree, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return true
	}
	var walk func(*syntax.Regexp) bool
	walk = func(n *syntax.Regexp) bool {
		switch n.Op {
		case syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText:
			return true
		}
		for _, sub := range n.Sub {
			if walk(sub) {
				return true
			}
		}
		return false
	}
	return walk(tree)
}

func relSlash(root, full string) string {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return filepath.ToSlash(full)
	}
	return filepath.ToSlash(rel)
}

// depth is the number of path elements in a slash-separated relative path.
func depth(rel string) int {
	if rel == "." || rel == "" {
		return 0
	}
	return strings.Count(rel, "/") + 1
}

func validGlob(g string) error {
	if g == "" {
		return nil
	}
	for _, seg := range strings.Split(g, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("invalid glob %q: %w", g, err)
		}
	}
	return nil
}

// matchGlob matches a slash-separated relative path. A pattern without "/"
// matches the base name at any depth (like rg -g "*.go"); otherwise it is
// matched against the whole relative path, and "**" matches any number of
// directories, including none.
func matchGlob(g, rel string) bool {
	if g == "" {
		return true
	}
	if !strings.Contains(g, "/") {
		ok, _ := path.Match(g, path.Base(rel))
		return ok
	}
	return matchSegments(strings.Split(strings.TrimPrefix(g, "/"), "/"), strings.Split(rel, "/"))
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for len(pattern) > 1 && pattern[1] == "**" {
				pattern = pattern[1:]
			}
			if len(pattern) == 1 {
				return true
			}
			for i := range len(name) + 1 {
				if matchSegments(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pattern[0], name[0]); !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}

func excluded(globs []string, rel string) bool {
	for _, g := range globs {
		if matchGlob(g, rel) {
			return true
		}
	}
	return false
}
