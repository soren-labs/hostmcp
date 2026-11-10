package host

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/soren-labs/rmc/internal/protocol"
)

// MoveInput renames one directory entry.
type MoveInput struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Cwd       string `json:"cwd,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

// movePath renames a file, directory or symlink within one filesystem, as
// rename(2) does: the entry keeps its inode, owner, mode and links, and a
// final symlink is renamed rather than followed. Without overwrite an
// existing destination is never replaced, even one that appears meanwhile.
func (r *Runtime) movePath(in MoveInput) (protocol.Result, error) {
	if in.From == "" || in.To == "" {
		return protocol.Result{}, errors.New("from and to are required")
	}
	from, err := r.path(in.From, in.Cwd)
	if err != nil {
		return protocol.Result{}, err
	}
	to, err := r.path(in.To, in.Cwd)
	if err != nil {
		return protocol.Result{}, err
	}
	if _, err := os.Lstat(from); err != nil {
		return protocol.Result{}, err
	}
	replaced := false
	if in.Overwrite {
		_, statErr := os.Lstat(to)
		replaced = statErr == nil
		err = os.Rename(from, to)
	} else {
		err = renameNoReplace(from, to)
	}
	switch {
	case err == nil:
	case errors.Is(err, syscall.EXDEV):
		return protocol.Result{}, fmt.Errorf("%s and %s are on different filesystems; move_path only renames, so copy the file instead: %w", from, to, err)
	case !in.Overwrite && errors.Is(err, os.ErrExist):
		return protocol.Result{}, fmt.Errorf("%s already exists; nothing was moved. Set overwrite=true to replace it: %w", to, err)
	default:
		return protocol.Result{}, err
	}
	return protocol.JSONResult(map[string]any{"from": from, "to": to, "replaced": replaced}), nil
}

// linkRename is the fallback for filesystems without an exclusive rename:
// link(2) never replaces an existing name and, like rename, does not follow a
// final symlink. It cannot move directories.
func linkRename(from, to string, cause error) error {
	info, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("this filesystem cannot rename %s without possibly replacing %s; set overwrite=true to allow replacing: %w", from, to, cause)
	}
	if err := os.Link(from, to); err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: errors.Unwrap(err)}
	}
	return os.Remove(from)
}
