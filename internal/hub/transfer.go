package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/soren-labs/rmc/internal/config"
	"github.com/soren-labs/rmc/internal/protocol"
)

// transfer_file copies a regular file between two devices one client may use,
// or within one device, without its content passing through the MCP client.
//
// The Hub still never touches a filesystem or runs a command itself: it makes
// ordinary read, write_file, move_path and apply_patch calls as the client's
// principal, so each chunk is authorized, revocation-checked, gated on the
// node's advertised features and logged like any other tool call. The target
// is assembled in a hidden staging file beside it and renamed into place, so
// it never appears half-written.
//
// An MCP request ends after requestDeadline, but a large file can take longer,
// so a transfer runs as a background job owned by its principal. A call waits
// up to wait_seconds and otherwise returns a transfer_id to wait on or cancel.

// A transfer keeps at most one read and one write in flight, so the running
// limit leaves at least half of a node's 16 request slots for other calls
// even if every transfer uses the same node.
const (
	transferMaxRunning   = 4                // across all principals
	transferMaxPerClient = 2                // running, per principal
	transferMaxKept      = 32               // remembered, per principal
	transferKeep         = 15 * time.Minute // a finished transfer stays queryable
	transferDefaultWait  = 30 * time.Second
	transferMaxWait      = 40 * time.Second // inside requestDeadline
	transferHashLimit    = 16 << 20         // nodes report a whole-file sha256 up to this size
	transferStagingName  = ".rmc-transfer-"
)

// transferTool is the MCP definition of the Hub's own transfer_file tool.
var transferTool = protocol.Tool{
	Name: "transfer_file",
	Description: "Copy a regular file from one device to another, or within one device, through the Hub, without its content passing through this conversation. " +
		"The Hub reads the source in 1 MiB chunks and appends them to a hidden staging file beside the target (.rmc-transfer-ID), then renames it into place, so the target never appears half-written; on failure or cancel the staging file is removed. " +
		"Each chunk is an ordinary read or write_file call with this client's authority on both devices, which must run a node that supports read offset, write_file append and move_path. " +
		"The target gets the source's permission bits unless mode is given; files up to 16 MiB are verified by sha256 end to end, larger ones by size. " +
		"Returns when done or after wait_seconds; while it runs, call again with only transfer_id to wait for it, or with cancel=true to stop it. Finished transfers are remembered for 15 minutes.",
	InputSchema: map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"source_device":  map[string]any{"type": "string", "minLength": 1, "maxLength": 64, "description": "Device to copy from, as listed by list_devices"},
			"source_path":    map[string]any{"type": "string", "description": "Regular file on the source device; symlinks are followed"},
			"target_device":  map[string]any{"type": "string", "minLength": 1, "maxLength": 64, "description": "Device to copy to; may equal source_device"},
			"target_path":    map[string]any{"type": "string", "description": "Destination file path on the target device. Ending it with / keeps the source file name in that directory"},
			"overwrite":      map[string]any{"type": "boolean", "description": "Atomically replace an existing target file"},
			"create_parents": map[string]any{"type": "boolean", "description": "Create missing parent directories of the target"},
			"mode":           map[string]any{"type": "string", "description": "Octal permissions for the target, e.g. 0644; default the source's permission bits. Like write_file mode, the target account's umask applies"},
			"wait_seconds":   map[string]any{"type": "integer", "minimum": 0, "maximum": int(transferMaxWait / time.Second), "description": "Wait up to this long for the transfer to finish; default 30"},
			"transfer_id":    map[string]any{"type": "string", "description": "Wait for, or with cancel stop, a transfer this client started; give no source or target with it"},
			"cancel":         map[string]any{"type": "boolean", "description": "With transfer_id, stop the transfer and remove its staging file"},
		},
	},
}

type transferInput struct {
	SourceDevice  string `json:"source_device"`
	SourcePath    string `json:"source_path"`
	TargetDevice  string `json:"target_device"`
	TargetPath    string `json:"target_path"`
	Overwrite     bool   `json:"overwrite"`
	CreateParents bool   `json:"create_parents"`
	Mode          string `json:"mode"`
	WaitSeconds   *int   `json:"wait_seconds"`
	TransferID    string `json:"transfer_id"`
	Cancel        bool   `json:"cancel"`
}

type transfer struct {
	id, principal string
	in            transferInput
	staging       string
	cancel        context.CancelFunc
	done          chan struct{}

	mu       sync.Mutex
	staged   bool   // the staging file may exist
	status   string // running, done, failed or canceled
	size     int64  // -1 until the source was read
	written  int64
	sum      string
	verified string
	err      string
	partial  string // staging file left behind when cleanup failed
	started  time.Time
	finished time.Time
}

type transferState struct {
	mu      sync.Mutex
	jobs    map[string]*transfer
	running int
	chunk   int // bytes per read; a field so tests can shrink it
}

// Transfer implements the transfer_file tool for principal.
func (h *Hub) Transfer(ctx context.Context, principal string, raw json.RawMessage) (protocol.Result, error) {
	var in transferInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return protocol.Result{}, fmt.Errorf("invalid transfer_file arguments: %w", err)
	}
	wait := transferDefaultWait
	if in.WaitSeconds != nil {
		if *in.WaitSeconds < 0 || time.Duration(*in.WaitSeconds)*time.Second > transferMaxWait {
			return protocol.Result{}, fmt.Errorf("wait_seconds must be between 0 and %d", int(transferMaxWait/time.Second))
		}
		wait = time.Duration(*in.WaitSeconds) * time.Second
	}
	// Answer before the MCP request itself is cut off.
	if deadline, ok := ctx.Value(requestDeadlineKey{}).(time.Time); ok {
		wait = min(wait, max(0, time.Until(deadline)-3*time.Second))
	}
	var t *transfer
	if in.TransferID != "" {
		if in.SourceDevice != "" || in.SourcePath != "" || in.TargetDevice != "" || in.TargetPath != "" || in.Overwrite || in.CreateParents || in.Mode != "" {
			return protocol.Result{}, errors.New("give only transfer_id (with wait_seconds or cancel) to follow an existing transfer")
		}
		if t = h.findTransfer(principal, in.TransferID); t == nil {
			return protocol.Result{}, fmt.Errorf("unknown transfer_id %q; finished transfers are remembered for %v", in.TransferID, transferKeep)
		}
		if in.Cancel {
			t.cancel()
		}
	} else {
		if in.Cancel {
			return protocol.Result{}, errors.New("cancel needs the transfer_id of a running transfer")
		}
		var err error
		if t, err = h.startTransfer(principal, in); err != nil {
			return protocol.Result{}, err
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-t.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	return t.result(), nil
}

func (h *Hub) findTransfer(principal, id string) *transfer {
	h.transfers.mu.Lock()
	defer h.transfers.mu.Unlock()
	h.pruneTransfersLocked()
	if t := h.transfers.jobs[id]; t != nil && t.principal == principal {
		return t
	}
	return nil
}

// pruneTransfersLocked forgets expired transfers and, per principal, the
// oldest finished ones beyond transferMaxKept.
func (h *Hub) pruneTransfersLocked() {
	perPrincipal := map[string][]*transfer{}
	for id, t := range h.transfers.jobs {
		t.mu.Lock()
		finished := t.finished
		t.mu.Unlock()
		if !finished.IsZero() && time.Since(finished) > transferKeep {
			delete(h.transfers.jobs, id)
			continue
		}
		if !finished.IsZero() {
			perPrincipal[t.principal] = append(perPrincipal[t.principal], t)
		}
	}
	for _, list := range perPrincipal {
		for len(list) > transferMaxKept {
			oldest := 0
			for i, t := range list {
				if t.finishedAt().Before(list[oldest].finishedAt()) {
					oldest = i
				}
			}
			delete(h.transfers.jobs, list[oldest].id)
			list = append(list[:oldest], list[oldest+1:]...)
		}
	}
}

func (t *transfer) finishedAt() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finished
}

// transfersRunning reports how many transfers are in progress.
func (h *Hub) transfersRunning() int {
	h.transfers.mu.Lock()
	defer h.transfers.mu.Unlock()
	return h.transfers.running
}

func validTransferPath(name, p string) error {
	if p == "" {
		return fmt.Errorf("%s is required", name)
	}
	if strings.ContainsAny(p, "\x00\r\n") {
		return fmt.Errorf("%s must not contain NUL or line breaks", name)
	}
	return nil
}

// transferDir returns the directory part of a native path without cleaning
// it, as the node does, so "link/.." keeps its kernel meaning.
func transferDir(p string) string {
	trimmed := strings.TrimRight(p, "/")
	i := strings.LastIndexByte(trimmed, '/')
	switch {
	case i < 0:
		return "."
	case i == 0:
		return "/"
	}
	return trimmed[:i]
}

func transferJoin(dir, name string) string {
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

func (h *Hub) startTransfer(principal string, in transferInput) (*transfer, error) {
	for _, f := range []struct{ name, value string }{{"source_device", in.SourceDevice}, {"source_path", in.SourcePath}, {"target_device", in.TargetDevice}, {"target_path", in.TargetPath}} {
		if err := validTransferPath(f.name, f.value); err != nil {
			return nil, err
		}
	}
	if strings.HasSuffix(in.TargetPath, "/") {
		base := in.SourcePath[strings.LastIndexByte(strings.TrimRight(in.SourcePath, "/"), '/')+1:]
		base = strings.TrimRight(base, "/")
		if base == "" || base == "." || base == ".." {
			return nil, errors.New("target_path ends with / but source_path has no file name to keep")
		}
		in.TargetPath += base
	}
	if in.Mode != "" {
		if n, err := strconv.ParseUint(in.Mode, 8, 32); err != nil || n > 0777 {
			return nil, errors.New("mode must be octal permissions between 0000 and 0777")
		}
	}
	cfg, err := h.store.Snapshot()
	if err != nil {
		return nil, errors.New("configuration unavailable")
	}
	client, err := h.clientIn(cfg, principal)
	if err != nil {
		return nil, err
	}
	// Refuse up front what the chunk calls would refuse halfway through.
	required := []struct {
		device, tool string
		args         map[string]any
	}{
		{in.SourceDevice, "read", map[string]any{"paths": []string{}, "encoding": "base64", "offset": 0, "max_bytes": 1024}},
		{in.TargetDevice, "write_file", map[string]any{"path": "", "content": "", "encoding": "base64", "append": true, "expected_size": 0, "create_parents": true, "mode": "0600"}},
		{in.TargetDevice, "move_path", map[string]any{"from": "", "to": "", "overwrite": true}},
		{in.TargetDevice, "apply_patch", map[string]any{"patch": ""}},
	}
	for _, r := range required {
		if !config.Allows(client, r.device) {
			return nil, fmt.Errorf("client is not authorized for device %q; nothing was transferred. %s", r.device, h.devicesHint(cfg, client))
		}
		h.mu.RLock()
		d := h.connections[r.device]
		h.mu.RUnlock()
		if d == nil {
			return nil, fmt.Errorf("device %q is offline; nothing was transferred. %s", r.device, h.devicesHint(cfg, client))
		}
		args, _ := json.Marshal(r.args)
		if feature := h.unsupportedFeature(d, r.tool, args); feature != "" {
			return nil, fmt.Errorf("device %s runs RMC node %s, which does not support %s needed by transfer_file; upgrade that node. Nothing was transferred", r.device, nodeVersion(d), feature)
		}
	}
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(h.ctx)
	t := &transfer{
		id: hex.EncodeToString(idBytes), principal: principal, in: in, cancel: cancel, done: make(chan struct{}),
		status: "running", size: -1, started: time.Now(),
	}
	t.staging = transferJoin(transferDir(in.TargetPath), transferStagingName+t.id)
	h.transfers.mu.Lock()
	h.pruneTransfersLocked()
	mine := 0
	for _, other := range h.transfers.jobs {
		if other.principal == principal && other.finishedAt().IsZero() {
			mine++
		}
	}
	switch {
	case mine >= transferMaxPerClient:
		h.transfers.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("this client already runs %d transfers; wait for one to finish", mine)
	case h.transfers.running >= transferMaxRunning:
		h.transfers.mu.Unlock()
		cancel()
		return nil, errors.New("the Hub is running its maximum number of transfers; try again shortly")
	}
	if h.transfers.jobs == nil {
		h.transfers.jobs = map[string]*transfer{}
	}
	h.transfers.jobs[t.id] = t
	h.transfers.running++
	chunk := h.transfers.chunk
	h.transfers.mu.Unlock()
	if chunk <= 0 {
		chunk = 1 << 20
	}
	log.Printf("event=transfer outcome=started transfer=%s principal=%q source_device=%q target_device=%q", t.id, auditPrincipal(principal), in.SourceDevice, in.TargetDevice)
	go h.runTransfer(ctx, t, chunk)
	return t, nil
}

// result is the transfer's current state as a tool result. A failed or
// canceled transfer is a tool error that still carries the details.
func (t *transfer) result() protocol.Result {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]any{
		"transfer_id": t.id, "status": t.status,
		"source":        map[string]string{"device": t.in.SourceDevice, "path": t.in.SourcePath},
		"target":        map[string]string{"device": t.in.TargetDevice, "path": t.in.TargetPath},
		"bytes_written": t.written, "started_at": t.started.UTC(),
	}
	end := time.Now()
	if !t.finished.IsZero() {
		end = t.finished
		out["finished_at"] = t.finished.UTC()
	}
	out["elapsed_ms"] = end.Sub(t.started).Milliseconds()
	if t.size >= 0 {
		out["size"] = t.size
	}
	switch t.status {
	case "running":
		out["note"] = "still copying; call transfer_file with this transfer_id to wait for it, or add cancel=true to stop it"
	case "done":
		out["sha256"], out["verified"] = t.sum, t.verified
	}
	if t.partial != "" {
		out["partial_path"] = t.partial
	}
	res := protocol.JSONResult(out)
	if t.err != "" {
		res.Error = "transfer " + t.status + ": " + t.err
	}
	return res
}

// readEntry is the part of a read result entry a transfer uses.
type readEntry struct {
	Type       string `json:"type"`
	Size       int64  `json:"size"`
	Mode       string `json:"mode"`
	ModifiedAt string `json:"modified_at"`
	SHA256     string `json:"sha256"`
	Content    string `json:"content"`
	Offset     int64  `json:"offset"`
	Truncated  bool   `json:"truncated"`
}

// nodeCall makes one native tool call as the transfer's principal.
func (h *Hub) nodeCall(ctx context.Context, t *transfer, device, tool string, args map[string]any, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	res, err := h.Call(ctx, t.principal, device, tool, raw)
	if err == nil && res.Error != "" {
		err = errors.New(res.Error)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s on %s: %w", tool, device, err)
	}
	if out != nil {
		if err := json.Unmarshal(res.Data, out); err != nil {
			return fmt.Errorf("%s on %s returned an unexpected result: %w", tool, device, err)
		}
	}
	return nil
}

func (h *Hub) readChunk(ctx context.Context, t *transfer, offset int64, chunk int) (readEntry, []byte, error) {
	var out struct {
		Entries []readEntry `json:"entries"`
	}
	err := h.nodeCall(ctx, t, t.in.SourceDevice, "read", map[string]any{"paths": []string{t.in.SourcePath}, "encoding": "base64", "offset": offset, "max_bytes": chunk}, &out)
	if err != nil {
		return readEntry{}, nil, err
	}
	if len(out.Entries) != 1 {
		return readEntry{}, nil, errors.New("read returned no entry for source_path")
	}
	e := out.Entries[0]
	if e.Type != "file" {
		return e, nil, fmt.Errorf("source_path is a %s, not a regular file", e.Type)
	}
	data, err := base64.StdEncoding.DecodeString(e.Content)
	if err != nil {
		return e, nil, errors.New("read returned invalid base64")
	}
	return e, data, nil
}

// permOf extracts permission bits from a Go FileMode string such as
// "-rwxr-xr-x" or "urwsr-xr-x"; set-ID and sticky bits are not copied.
func permOf(mode string) (string, bool) {
	if len(mode) < 9 {
		return "", false
	}
	var perm uint32
	for i, c := range mode[len(mode)-9:] {
		if c != '-' && c != 'S' && c != 'T' {
			perm |= 1 << (8 - i)
		}
	}
	return fmt.Sprintf("%04o", perm), true
}

type transferChunk struct {
	offset int64
	data   []byte
}

func (h *Hub) runTransfer(ctx context.Context, t *transfer, chunk int) {
	defer t.cancel()
	status, err := "done", h.copyFile(ctx, t, chunk)
	if err != nil {
		status = "failed"
		if errors.Is(err, context.Canceled) && h.ctx.Err() == nil {
			status, err = "canceled", errors.New("canceled by the client")
		} else if h.ctx.Err() != nil {
			err = errors.New("the Hub stopped during the transfer")
		}
	}
	partial := ""
	if err != nil {
		partial = h.removeStaging(t)
	}
	t.mu.Lock()
	t.status, t.finished, t.partial = status, time.Now(), partial
	if err != nil {
		t.err = err.Error()
		if partial != "" {
			t.err += "; the staging file " + partial + " could not be removed"
		}
	}
	size, written, elapsed := t.size, t.written, t.finished.Sub(t.started)
	t.mu.Unlock()
	h.transfers.mu.Lock()
	h.transfers.running--
	h.transfers.mu.Unlock()
	close(t.done)
	log.Printf("event=transfer outcome=%s transfer=%s principal=%q source_device=%q target_device=%q size=%d written=%d elapsed_ms=%d", status, t.id, auditPrincipal(t.principal), t.in.SourceDevice, t.in.TargetDevice, size, written, elapsed.Milliseconds())
}

// removeStaging deletes the staging file after a failure and returns its path
// if it may still exist.
func (h *Hub) removeStaging(t *transfer) string {
	t.mu.Lock()
	staged := t.staged
	t.mu.Unlock()
	if !staged {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	patch := "*** Begin Patch\n*** Delete File: " + t.staging + "\n*** End Patch\n"
	err := h.nodeCall(ctx, t, t.in.TargetDevice, "apply_patch", map[string]any{"patch": patch}, nil)
	if err != nil && !strings.Contains(err.Error(), "no such file or directory") {
		return t.staging
	}
	return ""
}

func (h *Hub) copyFile(ctx context.Context, t *transfer, chunk int) error {
	first, data, err := h.readChunk(ctx, t, 0, chunk)
	if err != nil {
		return err
	}
	size := first.Size
	t.mu.Lock()
	t.size = size
	t.mu.Unlock()
	mode := t.in.Mode
	if mode == "" {
		var ok bool
		if mode, ok = permOf(first.Mode); !ok {
			mode = "0644"
		}
	}
	// Refuse an existing target before copying anything; move_path still
	// refuses one that appears meanwhile.
	var probe struct {
		Entries []readEntry `json:"entries"`
	}
	err = h.nodeCall(ctx, t, t.in.TargetDevice, "read", map[string]any{"paths": []string{t.in.TargetPath}, "encoding": "base64", "max_bytes": 1024}, &probe)
	switch {
	case err == nil && len(probe.Entries) == 1 && probe.Entries[0].Type == "directory":
		return errors.New("target_path is a directory; give a file path, or end it with / to keep the source file name")
	case err == nil && !t.in.Overwrite:
		return errors.New("target_path already exists; set overwrite=true to replace it")
	case err != nil && ctx.Err() != nil:
		return ctx.Err()
	case err != nil && !strings.Contains(err.Error(), "no such file or directory"):
		return err
	}

	// Read ahead while the previous chunk is written.
	chunks := make(chan transferChunk, 2)
	readErr := make(chan error, 1)
	hash := sha256.New()
	hash.Write(data)
	go func() {
		defer close(chunks)
		offset := int64(len(data))
		select {
		case chunks <- transferChunk{0, data}:
		case <-ctx.Done():
			readErr <- ctx.Err()
			return
		}
		for offset < size {
			e, b, err := h.readChunk(ctx, t, offset, chunk)
			if err == nil && (e.Size != size || e.ModifiedAt != first.ModifiedAt || e.SHA256 != first.SHA256) {
				err = errors.New("source_path changed during the transfer; nothing was installed")
			}
			if err == nil && (len(b) == 0 || offset+int64(len(b)) > size) {
				err = errors.New("source_path returned an unexpected chunk; nothing was installed")
			}
			if err != nil {
				readErr <- err
				return
			}
			hash.Write(b)
			select {
			case chunks <- transferChunk{offset, b}:
			case <-ctx.Done():
				readErr <- ctx.Err()
				return
			}
			offset += int64(len(b))
		}
		readErr <- nil
	}()

	var last struct {
		Size   *int64 `json:"size"`
		SHA256 string `json:"sha256"`
	}
	for c := range chunks {
		args := map[string]any{"path": t.staging, "content": base64.StdEncoding.EncodeToString(c.data), "encoding": "base64"}
		if c.offset == 0 {
			t.mu.Lock()
			t.staged = true
			t.mu.Unlock()
			args["mode"] = mode
			if t.in.CreateParents {
				args["create_parents"] = true
			}
		} else {
			args["append"], args["expected_size"] = true, c.offset
		}
		last.Size, last.SHA256 = nil, ""
		if err := h.nodeCall(ctx, t, t.in.TargetDevice, "write_file", args, &last); err != nil {
			// Unblock and stop the reader before reporting.
			t.cancel()
			for range chunks {
			}
			return err
		}
		t.mu.Lock()
		t.written = c.offset + int64(len(c.data))
		t.mu.Unlock()
	}
	if err := <-readErr; err != nil {
		return err
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	t.mu.Lock()
	written := t.written
	t.mu.Unlock()
	if written != size || last.Size != nil && *last.Size != size {
		return fmt.Errorf("the staging file has %d bytes, not %d; nothing was installed", written, size)
	}
	verified := "size"
	if size <= transferHashLimit {
		if first.SHA256 != sum || last.SHA256 != sum {
			return errors.New("sha256 of the copy does not match the source; nothing was installed")
		}
		verified = "sha256"
	}
	if err := h.nodeCall(ctx, t, t.in.TargetDevice, "move_path", map[string]any{"from": t.staging, "to": t.in.TargetPath, "overwrite": t.in.Overwrite}, nil); err != nil {
		return err
	}
	t.mu.Lock()
	t.sum, t.verified = sum, verified
	t.mu.Unlock()
	return nil
}
