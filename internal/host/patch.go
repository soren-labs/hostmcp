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

	"github.com/soren-labs/hostmcp/internal/protocol"
)

type PatchInput struct {
	Patch string `json:"patch"`
	Cwd   string `json:"cwd,omitempty"`
}
type patchHunk struct {
	anchor   string
	old, new []string
	eof      bool
}
type patchOp struct {
	kind, path, move string
	added            []string
	hunks            []patchHunk
}
type patchPlan struct {
	op             patchOp
	source, dest   string
	before, after  []byte
	mode           os.FileMode
	symlink        bool
	linkTarget     string
	added, removed int
}

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
		}
		if op.kind == "update" {
			if i < len(lines)-1 && strings.HasPrefix(lines[i], "*** Move to: ") {
				op.move = strings.TrimPrefix(lines[i], "*** Move to: ")
				if op.move == "" {
					return nil, errors.New("move destination must not be empty")
				}
				i++
			}
			for i < len(lines)-1 && (lines[i] == "@@" || strings.HasPrefix(lines[i], "@@ ")) {
				h := patchHunk{}
				if lines[i] != "@@" {
					h.anchor = lines[i][3:]
				}
				i++
				count := 0
				for i < len(lines)-1 && !strings.HasPrefix(lines[i], "*** ") && lines[i] != "@@" && !strings.HasPrefix(lines[i], "@@ ") {
					line := lines[i]
					if len(line) == 0 {
						return nil, fmt.Errorf("empty hunk line %d must have a space, + or - prefix", i+1)
					}
					switch line[0] {
					case ' ':
						h.old = append(h.old, line[1:])
						h.new = append(h.new, line[1:])
					case '-':
						h.old = append(h.old, line[1:])
					case '+':
						h.new = append(h.new, line[1:])
					default:
						return nil, fmt.Errorf("invalid hunk line %d", i+1)
					}
					count++
					i++
				}
				if i < len(lines)-1 && lines[i] == "*** End of File" {
					h.eof = true
					i++
				}
				if count == 0 {
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

func readRegularBounded(path string) ([]byte, os.FileMode, error) {
	f, err := openRegular(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > 4<<20 {
		return nil, 0, fmt.Errorf("%s exceeds the 4 MiB patch input limit", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil {
		return nil, 0, err
	}
	if len(b) > 4<<20 {
		return nil, 0, errors.New("patch input grew beyond 4 MiB")
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return nil, 0, fmt.Errorf("%s is binary; use write_file or a shell command", path)
	}
	return b, info.Mode().Perm(), nil
}

func matchLines(lines, needle []string, from int, eof bool) (int, error) {
	if len(needle) == 0 {
		if eof {
			return len(lines), nil
		}
		return from, nil
	}
	match := -1
	for i := from; i+len(needle) <= len(lines); i++ {
		if eof && i+len(needle) != len(lines) {
			continue
		}
		ok := true
		for j := range needle {
			if lines[i+j] != needle[j] {
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
func patchContent(before []byte, hunks []patchHunk) ([]byte, int, int, error) {
	text := string(before)
	crlf := strings.Contains(text, "\r\n")
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	trailing := strings.HasSuffix(normalized, "\n")
	var lines []string
	if normalized != "" {
		lines = strings.Split(strings.TrimSuffix(normalized, "\n"), "\n")
	}
	cursor, added, removed := 0, 0, 0
	for _, h := range hunks {
		if h.anchor != "" {
			pos, err := matchLines(lines, []string{h.anchor}, cursor, false)
			if err != nil {
				return nil, 0, 0, fmt.Errorf("anchor %q: %w", h.anchor, err)
			}
			cursor = pos + 1
		}
		pos, err := matchLines(lines, h.old, cursor, h.eof)
		if err != nil {
			return nil, 0, 0, err
		}
		newLines := make([]string, 0, len(lines)-len(h.old)+len(h.new))
		newLines = append(newLines, lines[:pos]...)
		newLines = append(newLines, h.new...)
		newLines = append(newLines, lines[pos+len(h.old):]...)
		lines = newLines
		cursor = pos + len(h.new)
		added += len(h.new)
		removed += len(h.old)
	}
	end := ""
	if len(lines) > 0 && (trailing || len(before) == 0) {
		end = "\n"
	}
	out := strings.Join(lines, "\n") + end
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if len(out) > 4<<20 {
		return nil, 0, 0, errors.New("patched file exceeds 4 MiB")
	}
	return []byte(out), added, removed, nil
}

func (r *Runtime) applyPatch(in PatchInput) (protocol.Result, error) {
	ops, err := parsePatch(in.Patch)
	if err != nil {
		return protocol.Result{}, err
	}
	plans := make([]patchPlan, 0, len(ops))
	touched := map[string]bool{}
	totalBytes := 0
	for _, op := range ops {
		path, err := r.path(op.path, in.Cwd)
		if err != nil {
			return protocol.Result{}, err
		}
		plan := patchPlan{op: op, source: path, dest: path, mode: 0644}
		if touched[path] {
			return protocol.Result{}, fmt.Errorf("path appears more than once in patch: %s", path)
		}
		touched[path] = true
		info, statErr := os.Lstat(path)
		if op.kind == "add" {
			if statErr == nil {
				return protocol.Result{}, fmt.Errorf("add target already exists: %s", path)
			}
			if !errors.Is(statErr, os.ErrNotExist) {
				return protocol.Result{}, statErr
			}
			plan.after = []byte(strings.Join(op.added, "\n"))
			if len(op.added) > 0 {
				plan.after = append(plan.after, '\n')
			}
			plan.added = len(op.added)
		} else {
			if statErr != nil {
				return protocol.Result{}, statErr
			}
			plan.symlink = info.Mode()&os.ModeSymlink != 0
			if plan.symlink {
				plan.linkTarget, err = os.Readlink(path)
				if err != nil {
					return protocol.Result{}, err
				}
			}
			if op.kind == "delete" && plan.symlink {
				plan.before = []byte(plan.linkTarget)
			} else {
				plan.before, plan.mode, err = readRegularBounded(path)
				if err != nil {
					return protocol.Result{}, err
				}
			}
			if op.kind == "update" {
				plan.after, plan.added, plan.removed, err = patchContent(plan.before, op.hunks)
				if err != nil {
					return protocol.Result{}, fmt.Errorf("%s: %w", path, err)
				}
				if plan.symlink {
					if op.move != "" {
						return protocol.Result{}, errors.New("moving a symlink with apply_patch is unsupported; use an explicit shell operation")
					}
					plan.dest, err = filepath.EvalSymlinks(path)
					if err != nil {
						return protocol.Result{}, err
					}
					if touched[plan.dest] && plan.dest != path {
						return protocol.Result{}, errors.New("patch aliases the same file through a symlink")
					}
					touched[plan.dest] = true
				}
			}
		}
		if op.move != "" {
			plan.dest, err = r.path(op.move, in.Cwd)
			if err != nil {
				return protocol.Result{}, err
			}
			if touched[plan.dest] {
				return protocol.Result{}, fmt.Errorf("move destination conflicts with another patch path: %s", plan.dest)
			}
			touched[plan.dest] = true
			if _, e := os.Lstat(plan.dest); !errors.Is(e, os.ErrNotExist) {
				if e == nil {
					return protocol.Result{}, fmt.Errorf("move destination exists: %s", plan.dest)
				}
				return protocol.Result{}, e
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
		if err = checkPatchUnchanged(plan); err == nil {
			switch plan.op.kind {
			case "delete":
				err = os.Remove(plan.source)
			case "add", "update":
				if err = os.MkdirAll(filepath.Dir(plan.dest), 0755); err == nil {
					err = atomicWrite(plan.dest, plan.after, plan.mode, plan.op.kind == "add" || plan.op.move != "", plan.op.kind == "add")
				}
				if err == nil && plan.op.move != "" {
					if e := os.Remove(plan.source); e != nil {
						completed = append(completed, map[string]any{"action": "created_move_destination", "path": plan.dest})
						err = fmt.Errorf("destination written but could not remove source %s: %w", plan.source, e)
					}
				}
			}
		}
		if err != nil {
			out := protocol.JSONResult(map[string]any{"applied": completed, "failed_path": plan.source, "atomic_across_files": false})
			out.Error = fmt.Sprintf("patch commit failed after %d completed operations: %v", len(completed), err)
			return out, nil
		}
		change := map[string]any{"action": plan.op.kind, "path": plan.source}
		if plan.op.move != "" {
			change["move_to"] = plan.dest
		}
		completed = append(completed, change)
	}
	return protocol.JSONResult(map[string]any{"applied": completed, "atomic_across_files": false}), nil
}

func checkPatchUnchanged(p patchPlan) error {
	if p.op.kind == "add" {
		if _, err := os.Lstat(p.dest); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		return fmt.Errorf("add target appeared while planning patch: %s", p.dest)
	}
	if p.symlink {
		target, err := os.Readlink(p.source)
		if err != nil {
			return err
		}
		if target != p.linkTarget {
			return errors.New("symlink changed while planning patch")
		}
		if p.op.kind == "delete" {
			return nil
		}
	}
	now, _, err := readRegularBounded(p.source)
	if err != nil {
		return err
	}
	if !bytes.Equal(now, p.before) {
		return fmt.Errorf("file changed while planning patch: %s", p.source)
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
		name := filepath.Join(dir, fmt.Sprintf(".hostmcp-patch-%x", nonce))
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
		if !errors.Is(err, os.ErrExist) {
			return f, err
		}
	}
	return nil, errors.New("could not allocate a unique patch temporary file")
}

func atomicWrite(path string, data []byte, mode os.FileMode, noOverwrite, newFile bool) error {
	var f *os.File
	var err error
	if newFile {
		f, err = createPatchTempWithMode(filepath.Dir(path), mode)
	} else {
		f, err = os.CreateTemp(filepath.Dir(path), ".hostmcp-patch-*")
	}
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if !newFile {
		if err = f.Chmod(mode); err != nil {
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
	if noOverwrite {
		return os.Link(temp, path)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() {
		return fmt.Errorf("atomic update target %s is no longer a regular file", path)
	}
	return os.Rename(temp, path)
}
