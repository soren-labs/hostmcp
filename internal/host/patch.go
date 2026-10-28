package host

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/soren-labs/hostmcp/internal/protocol"
)

type PatchInput struct {
	Patch string `json:"patch"`
	Cwd   string `json:"cwd,omitempty"`
}
type patchHunk struct {
	anchor   string
	old, new []string
	// context maps each new line to the old line it repeats, or -1 for an
	// inserted line, so unchanged context keeps its exact original line ending.
	context []int
	eof     bool
}
type patchOp struct {
	kind, path, move string
	added            []string
	hunks            []patchHunk
	plus, minus      int
}
type fileIdentity struct{ dev, ino uint64 }
type patchPlan struct {
	op            patchOp
	source, dest  string
	before, after []byte
	identity      fileIdentity
	info          os.FileInfo
}

func isHunkHeader(line string) bool { return line == "@@" || strings.HasPrefix(line, "@@ ") }

func parsePatch(text string) ([]patchOp, error) {
	if len(text) > 1<<20 {
		return nil, errors.New("patch exceeds 1 MiB")
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 2 || lines[0] != "*** Begin Patch" || lines[len(lines)-1] != "*** End Patch" {
		return nil, errors.New("patch must start with *** Begin Patch and end with *** End Patch")
	}
	ops := []patchOp{}
	for i := 1; i < len(lines)-1; {
		var op patchOp
		switch {
		case strings.HasPrefix(lines[i], "*** Add File: "):
			op.kind = "add"
			op.path = strings.TrimPrefix(lines[i], "*** Add File: ")
		case strings.HasPrefix(lines[i], "*** Delete File: "):
			op.kind = "delete"
			op.path = strings.TrimPrefix(lines[i], "*** Delete File: ")
		case strings.HasPrefix(lines[i], "*** Update File: "):
			op.kind = "update"
			op.path = strings.TrimPrefix(lines[i], "*** Update File: ")
		default:
			return nil, fmt.Errorf("invalid patch header on line %d", i+1)
		}
		if op.path == "" {
			return nil, errors.New("patch path must not be empty")
		}
		i++
		if op.kind == "add" {
			for i < len(lines)-1 && !strings.HasPrefix(lines[i], "*** ") {
				if !strings.HasPrefix(lines[i], "+") {
					return nil, fmt.Errorf("added file line %d must start with +", i+1)
				}
				op.added = append(op.added, lines[i][1:])
				i++
			}
			op.plus = len(op.added)
		}
		if op.kind == "update" {
			if i < len(lines)-1 && strings.HasPrefix(lines[i], "*** Move to: ") {
				op.move = strings.TrimPrefix(lines[i], "*** Move to: ")
				if op.move == "" {
					return nil, errors.New("move destination must not be empty")
				}
				i++
			}
			// As in the upstream format, the first hunk may omit its @@ line and an
			// empty hunk line is an empty context line.
			for i < len(lines)-1 && !strings.HasPrefix(lines[i], "*** ") {
				if !isHunkHeader(lines[i]) && len(op.hunks) > 0 {
					return nil, fmt.Errorf("expected @@ before hunk line %d", i+1)
				}
				h := patchHunk{}
				if isHunkHeader(lines[i]) {
					if lines[i] != "@@" {
						h.anchor = lines[i][3:]
					}
					i++
				}
				for i < len(lines)-1 && !strings.HasPrefix(lines[i], "*** ") && !isHunkHeader(lines[i]) {
					line := lines[i]
					switch {
					case line == "" || line[0] == ' ':
						if line != "" {
							line = line[1:]
						}
						h.context = append(h.context, len(h.old))
						h.old = append(h.old, line)
						h.new = append(h.new, line)
					case line[0] == '-':
						h.old = append(h.old, line[1:])
						op.minus++
					case line[0] == '+':
						h.context = append(h.context, -1)
						h.new = append(h.new, line[1:])
						op.plus++
					default:
						return nil, fmt.Errorf("invalid hunk line %d", i+1)
					}
					i++
				}
				if i < len(lines)-1 && lines[i] == "*** End of File" {
					h.eof = true
					i++
				}
				if len(h.old) == 0 && len(h.new) == 0 {
					return nil, errors.New("empty update hunk")
				}
				op.hunks = append(op.hunks, h)
			}
			if len(op.hunks) == 0 && op.move == "" {
				return nil, fmt.Errorf("update %s has no hunks", op.path)
			}
		}
		ops = append(ops, op)
		if len(ops) > 32 {
			return nil, errors.New("a patch may modify at most 32 files")
		}
	}
	if len(ops) == 0 {
		return nil, errors.New("patch has no file operations")
	}
	return ops, nil
}

func identityOf(info os.FileInfo) fileIdentity {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return fileIdentity{dev: uint64(st.Dev), ino: uint64(st.Ino)}
	}
	return fileIdentity{}
}

func readBounded(f *os.File, path string) ([]byte, error) {
	b, err := io.ReadAll(io.NewSectionReader(f, 0, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 4<<20 {
		return nil, fmt.Errorf("%s exceeds the 4 MiB patch input limit", path)
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return nil, fmt.Errorf("%s is binary; use write_file or a shell command", path)
	}
	return b, nil
}

func readRegularBounded(path string) ([]byte, os.FileInfo, error) {
	f, err := openRegular(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.Size() > 4<<20 {
		return nil, nil, fmt.Errorf("%s exceeds the 4 MiB patch input limit", path)
	}
	b, err := readBounded(f, path)
	return b, info, err
}

type sourceLine struct{ text, ending string }

// splitLines keeps each line's own terminator (LF, CRLF or CR). The first
// terminator in the file is used for inserted lines; LF if there is none.
func splitLines(b []byte) ([]sourceLine, string) {
	text := string(b)
	var lines []sourceLine
	preferred := ""
	start := 0
	for i := 0; i < len(text); i++ {
		ending := ""
		switch {
		case text[i] == '\n':
			ending = "\n"
		case text[i] == '\r' && i+1 < len(text) && text[i+1] == '\n':
			ending = "\r\n"
		case text[i] == '\r':
			ending = "\r"
		default:
			continue
		}
		if preferred == "" {
			preferred = ending
		}
		lines = append(lines, sourceLine{text: text[start:i], ending: ending})
		i += len(ending) - 1
		start = i + 1
	}
	if start < len(text) {
		lines = append(lines, sourceLine{text: text[start:]})
	}
	if preferred == "" {
		preferred = "\n"
	}
	return lines, preferred
}

// matchLines requires the context to occur exactly once at or after from. A
// stale or repeated context is refused instead of editing a guessed location.
func matchLines(lines []sourceLine, needle []string, from int, eof bool) (int, error) {
	match := -1
	for i := from; i+len(needle) <= len(lines); i++ {
		if eof && i+len(needle) != len(lines) {
			continue
		}
		ok := true
		for j := range needle {
			if lines[i+j].text != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			if match >= 0 {
				return 0, errors.New("ambiguous patch context; include more unchanged context")
			}
			match = i
		}
	}
	if match < 0 {
		return 0, errors.New("patch context does not match the current file")
	}
	return match, nil
}

func patchContent(before []byte, hunks []patchHunk) ([]byte, error) {
	lines, ending := splitLines(before)
	finalNewline := len(lines) == 0 || lines[len(lines)-1].ending != ""
	cursor := 0
	for _, h := range hunks {
		if h.anchor != "" {
			// An anchor names the first matching line after the previous hunk,
			// as in the upstream patch format; the hunk context below must
			// still be unique after it.
			pos := -1
			for i := cursor; i < len(lines); i++ {
				if lines[i].text == h.anchor {
					pos = i
					break
				}
			}
			if pos < 0 {
				return nil, fmt.Errorf("anchor %q not found after the previous hunk", h.anchor)
			}
			cursor = pos + 1
		}
		inserted := func(text string) sourceLine { return sourceLine{text: text, ending: ending} }
		if len(h.old) == 0 {
			// A hunk of only added lines appends at end of file, or directly
			// after its @@ anchor when one is given.
			at := len(lines)
			if h.anchor != "" && !h.eof {
				at = cursor
			}
			add := make([]sourceLine, 0, len(h.new))
			for _, text := range h.new {
				add = append(add, inserted(text))
			}
			lines = append(lines[:at], append(add, lines[at:]...)...)
			if at == cursor {
				cursor += len(add)
			}
			continue
		}
		old, newText, context := h.old, h.new, h.context
		pos, err := matchLines(lines, old, cursor, h.eof)
		if err != nil && old[len(old)-1] == "" && len(old) > 1 {
			// A trailing empty context line often stands for the file's final
			// newline, which is not a separate line here.
			trimmedNew, trimmedContext := newText, context
			if n := len(newText); n > 0 && newText[n-1] == "" && context[n-1] == len(old)-1 {
				trimmedNew, trimmedContext = newText[:n-1], context[:n-1]
			}
			if p, e := matchLines(lines, old[:len(old)-1], cursor, h.eof); e == nil {
				pos, err, old, newText, context = p, nil, old[:len(old)-1], trimmedNew, trimmedContext
			}
		}
		if err != nil {
			return nil, err
		}
		replacement := make([]sourceLine, 0, len(newText))
		for j, text := range newText {
			if k := context[j]; k >= 0 && k < len(old) {
				replacement = append(replacement, lines[pos+k])
			} else {
				replacement = append(replacement, inserted(text))
			}
		}
		rest := append([]sourceLine{}, lines[pos+len(old):]...)
		lines = append(append(lines[:pos], replacement...), rest...)
		cursor = pos + len(replacement)
	}
	var out strings.Builder
	for i, line := range lines {
		out.WriteString(line.text)
		switch {
		case i < len(lines)-1 && line.ending == "":
			out.WriteString(ending)
		case i == len(lines)-1 && !finalNewline:
			// Keep a file that had no final newline without one.
		case i == len(lines)-1 && line.ending == "":
			out.WriteString(ending)
		default:
			out.WriteString(line.ending)
		}
		if out.Len() > 4<<20 {
			return nil, errors.New("patched file exceeds 4 MiB")
		}
	}
	return []byte(out.String()), nil
}

// newPathKey identifies a path that does not exist yet by its resolved parent,
// so two spellings of one new file in a patch are detected before writing.
func newPathKey(path string) string {
	parent, leaf := nativeDir(path), path[strings.LastIndexByte(path, '/')+1:]
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		return "new:" + resolved + "/" + leaf
	}
	return "new:" + path
}

func checkNewLeaf(path string) error {
	leaf := path[strings.LastIndexByte(path, '/')+1:]
	if leaf == "" || leaf == "." || leaf == ".." {
		return fmt.Errorf("%s does not name a file", path)
	}
	return nil
}

func (r *Runtime) applyPatch(in PatchInput) (protocol.Result, error) {
	ops, err := parsePatch(in.Patch)
	if err != nil {
		return protocol.Result{}, err
	}
	plans := make([]patchPlan, 0, len(ops))
	claimed := map[string]string{}
	claim := func(key, path string) error {
		if previous, ok := claimed[key]; ok {
			return fmt.Errorf("patch refers to the same file more than once: %s and %s", previous, path)
		}
		claimed[key] = path
		return nil
	}
	totalBytes := 0
	for _, op := range ops {
		path, err := r.path(op.path, in.Cwd)
		if err != nil {
			return protocol.Result{}, err
		}
		plan := patchPlan{op: op, source: path, dest: path}
		switch op.kind {
		case "add":
			if err = checkNewLeaf(path); err != nil {
				return protocol.Result{}, err
			}
			if _, statErr := os.Lstat(path); statErr == nil {
				return protocol.Result{}, fmt.Errorf("add target already exists: %s", path)
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return protocol.Result{}, statErr
			}
			if err = claim(newPathKey(path), path); err != nil {
				return protocol.Result{}, err
			}
			plan.after = []byte(strings.Join(op.added, "\n"))
			if len(op.added) > 0 {
				plan.after = append(plan.after, '\n')
			}
		case "delete":
			// Deletion needs no content, so large and binary files can be removed.
			info, err := os.Lstat(path)
			if err != nil {
				return protocol.Result{}, err
			}
			if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
				return protocol.Result{}, fmt.Errorf("%s is not a regular file or symlink; use an explicit shell command", path)
			}
			plan.info, plan.identity = info, identityOf(info)
			if err = claim(fmt.Sprintf("inode:%d:%d", plan.identity.dev, plan.identity.ino), path); err != nil {
				return protocol.Result{}, err
			}
		case "update":
			if op.move != "" {
				if info, err := os.Lstat(path); err != nil {
					return protocol.Result{}, err
				} else if info.Mode()&os.ModeSymlink != 0 {
					return protocol.Result{}, errors.New("moving a symlink with apply_patch is unsupported; use an explicit shell operation")
				}
			}
			// Opening follows symlinks; the edit applies to the file they name.
			plan.before, plan.info, err = readRegularBounded(path)
			if err != nil {
				return protocol.Result{}, err
			}
			plan.identity = identityOf(plan.info)
			if err = claim(fmt.Sprintf("inode:%d:%d", plan.identity.dev, plan.identity.ino), path); err != nil {
				return protocol.Result{}, err
			}
			plan.after = plan.before
			if len(op.hunks) > 0 {
				if plan.after, err = patchContent(plan.before, op.hunks); err != nil {
					return protocol.Result{}, fmt.Errorf("%s: %w", path, err)
				}
			}
			if op.move != "" {
				if plan.dest, err = r.path(op.move, in.Cwd); err != nil {
					return protocol.Result{}, err
				}
				if err = checkNewLeaf(plan.dest); err != nil {
					return protocol.Result{}, err
				}
				if _, e := os.Lstat(plan.dest); e == nil {
					return protocol.Result{}, fmt.Errorf("move destination exists: %s", plan.dest)
				} else if !errors.Is(e, os.ErrNotExist) {
					return protocol.Result{}, e
				}
				if err = claim(newPathKey(plan.dest), plan.dest); err != nil {
					return protocol.Result{}, err
				}
			}
		}
		totalBytes += len(plan.before) + len(plan.after)
		if totalBytes > 8<<20 {
			return protocol.Result{}, errors.New("combined patch input and output exceeds 8 MiB")
		}
		plans = append(plans, plan)
	}
	completed := make([]map[string]any, 0, len(plans))
	for _, plan := range plans {
		switch plan.op.kind {
		case "delete":
			err = commitDelete(plan)
		case "add":
			if err = os.MkdirAll(nativeDir(plan.dest), 0755); err == nil {
				err = createFile(plan.dest, plan.after, 0644, nil)
			}
		case "update":
			if plan.op.move != "" {
				err = commitMove(plan)
			} else {
				err = rewriteInPlace(plan.source, plan)
			}
		}
		if err != nil {
			var partial *partialMoveError
			if errors.As(err, &partial) {
				completed = append(completed, map[string]any{"action": "created_move_destination", "path": plan.dest})
			}
			out := protocol.JSONResult(map[string]any{"applied": completed, "failed_path": plan.source, "atomic_across_files": false})
			out.Error = fmt.Sprintf("patch commit failed after %d completed operations: %v", len(completed), err)
			return out, nil
		}
		change := map[string]any{"action": plan.op.kind, "path": plan.source}
		if plan.op.move != "" {
			change["move_to"] = plan.dest
		}
		if plan.op.kind != "delete" {
			change["lines_added"], change["lines_removed"] = plan.op.plus, plan.op.minus
		}
		completed = append(completed, change)
	}
	return protocol.JSONResult(map[string]any{"applied": completed, "atomic_across_files": false}), nil
}

func commitDelete(p patchPlan) error {
	info, err := os.Lstat(p.source)
	if err != nil {
		return err
	}
	if identityOf(info) != p.identity {
		return fmt.Errorf("%s was replaced while planning patch", p.source)
	}
	return os.Remove(p.source)
}

// rewriteInPlace edits the existing inode through a descriptor, like an
// editor saving in place. Owner, group, full mode, hard links, ACLs and
// extended attributes stay as they are, and a file writable by this account
// can be patched even when its directory is not. The descriptor is checked to
// be the planned file with the planned content before any byte is written.
// Growth is reserved first where the filesystem supports it, and a failed
// write restores the original bytes.
func rewriteInPlace(path string, p patchPlan) (err error) {
	f, err := openRegular(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close %s after patching: %w", path, closeErr)
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if identityOf(info) != p.identity {
		return fmt.Errorf("%s was replaced while planning patch", path)
	}
	current, err := readBounded(f, path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, p.before) {
		return fmt.Errorf("file changed while planning patch: %s", path)
	}
	if bytes.Equal(p.after, p.before) {
		return nil
	}
	prefix := 0
	for prefix < len(p.before) && prefix < len(p.after) && p.before[prefix] == p.after[prefix] {
		prefix++
	}
	if len(p.after) > len(p.before) {
		if err = reserveSpace(f, int64(len(p.after))); err != nil {
			return fmt.Errorf("cannot reserve space to patch %s; the file is unchanged: %w", path, err)
		}
	}
	_, err = f.WriteAt(p.after[prefix:], int64(prefix))
	if err == nil && len(p.after) < len(p.before) {
		err = f.Truncate(int64(len(p.after)))
	}
	// POSIX write and truncate may clear setuid/setgid bits even when the
	// inode itself is retained. Restore the original mode using the already
	// verified descriptor rather than replacing the file on disk.
	if err == nil && fmodeNeedsRestore(info.Mode()) {
		err = f.Chmod(info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky))
	}
	if err != nil {
		_, restoreErr := f.WriteAt(p.before[prefix:], int64(prefix))
		if restoreErr == nil {
			restoreErr = f.Truncate(int64(len(p.before)))
		}
		if restoreErr == nil {
			restoreErr = f.Chmod(info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky))
		}
		if restoreErr == nil {
			restoreErr = f.Sync()
		}
		if restoreErr != nil {
			return fmt.Errorf("writing %s failed and its original content could not be restored (%v); inspect the file: %w", path, restoreErr, err)
		}
		return fmt.Errorf("writing %s failed; its original content was restored: %w", path, err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("patched content of %s was written but could not be flushed to disk: %w", path, err)
	}
	return nil
}

func fmodeNeedsRestore(mode os.FileMode) bool {
	return mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0
}

type partialMoveError struct{ err error }

func (e *partialMoveError) Error() string { return e.err.Error() }
func (e *partialMoveError) Unwrap() error { return e.err }

// commitMove gives the same inode its new name, as rename(2) and mv do, so the
// moved file keeps its owner, mode, links and attributes. link+unlink never
// replaces a destination that appeared after planning. Across filesystems the
// file is copied instead, keeping its mode and, when permitted, its owner.
func commitMove(p patchPlan) error {
	if err := os.MkdirAll(nativeDir(p.dest), 0755); err != nil {
		return err
	}
	linkErr := os.Link(p.source, p.dest)
	if linkErr == nil {
		if err := rewriteInPlace(p.dest, p); err != nil {
			if removeErr := os.Remove(p.dest); removeErr != nil {
				return &partialMoveError{fmt.Errorf("%w; the new name %s could not be removed: %v", err, p.dest, removeErr)}
			}
			return err
		}
		if err := os.Remove(p.source); err != nil {
			return &partialMoveError{fmt.Errorf("destination written but could not remove source %s: %w", p.source, err)}
		}
		return nil
	}
	if !errors.Is(linkErr, syscall.EXDEV) && !errors.Is(linkErr, syscall.EPERM) && !errors.Is(linkErr, syscall.ENOTSUP) && !errors.Is(linkErr, syscall.EMLINK) {
		return linkErr
	}
	current, info, err := readRegularBounded(p.source)
	if err != nil {
		return err
	}
	if identityOf(info) != p.identity || !bytes.Equal(current, p.before) {
		return fmt.Errorf("file changed while planning patch: %s", p.source)
	}
	if err = createFile(p.dest, p.after, 0600, info); err != nil {
		return err
	}
	if err = os.Remove(p.source); err != nil {
		return &partialMoveError{fmt.Errorf("destination written but could not remove source %s: %w", p.source, err)}
	}
	return nil
}

// CreateTemp always requests 0600. For a newly added file, request its normal
// creation mode directly so the kernel applies the current umask. Never inspect
// umask by changing the process-global value: other goroutines may create files.
func createPatchTempWithMode(dir string, mode os.FileMode) (*os.File, error) {
	for attempt := 0; attempt < 10; attempt++ {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		name := nativeJoin(dir, fmt.Sprintf(".hostmcp-patch-%x", nonce))
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
		if !errors.Is(err, os.ErrExist) {
			return f, err
		}
	}
	return nil, errors.New("could not allocate a unique patch temporary file")
}

// createFile writes a complete new file and installs it with link(2), which
// never replaces an existing destination. With like set, the copy takes that
// file's owner when permitted and its mode; set-ID bits are kept only if the
// owner was preserved, so a copy never gains another account's set-ID bit.
func createFile(path string, data []byte, mode os.FileMode, like os.FileInfo) error {
	f, err := createPatchTempWithMode(nativeDir(path), mode)
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if like != nil {
		perm := like.Mode() & (os.ModePerm | os.ModeSticky | os.ModeSetuid | os.ModeSetgid)
		if st, ok := like.Sys().(*syscall.Stat_t); ok {
			if f.Chown(int(st.Uid), int(st.Gid)) != nil {
				perm &^= os.ModeSetuid | os.ModeSetgid
			}
		}
		if err = f.Chmod(perm); err != nil {
			f.Close()
			return err
		}
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Link(temp, path)
}
