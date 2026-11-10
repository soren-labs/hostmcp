package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/soren-labs/rmc/internal/config"
	"github.com/soren-labs/rmc/internal/host"
	"github.com/soren-labs/rmc/internal/node"
	"github.com/soren-labs/rmc/internal/protocol"
)

const dev2Token = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

// hookedRuntime is a real native runtime whose calls can be delayed, failed
// or preceded by side effects.
type hookedRuntime struct {
	*host.Runtime
	hook func(tool string, args json.RawMessage) error
}

func (e *hookedRuntime) Call(ctx context.Context, p, tool string, args json.RawMessage) (protocol.Result, error) {
	if e.hook != nil {
		if err := e.hook(tool, args); err != nil {
			return protocol.Result{}, err
		}
	}
	return e.Runtime.Call(ctx, p, tool, args)
}

// oldRuntime advertises the tools of a node that predates move_path.
type oldRuntime struct{ *host.Runtime }

func (e oldRuntime) Tools() []protocol.Tool {
	var tools []protocol.Tool
	for _, tool := range host.Tools() {
		if tool.Name != "move_path" {
			tools = append(tools, tool)
		}
	}
	return tools
}

func newRuntime(t *testing.T) *host.Runtime {
	t.Helper()
	rt, err := host.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// transferFixture is a Hub with the native tool surface and two devices.
// alice may use both, bob only dev2.
func transferFixture(t *testing.T, chunk int) (*Hub, *httptest.Server) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.json")
	cfg := config.Hub{Version: config.FormatVersion, Listen: "127.0.0.1:0", PublicURL: "http://127.0.0.1", Clients: []config.Client{
		{ID: "alice", TokenHash: config.Hash(aliceToken), Devices: []string{"dev1", "dev2"}},
		{ID: "bob", TokenHash: config.Hash(bobToken), Devices: []string{"dev2"}},
	}, Devices: []config.Device{{ID: "dev1", TokenHash: config.Hash(deviceToken)}, {ID: "dev2", TokenHash: config.Hash(dev2Token)}}}
	if err := config.WriteJSON(path, cfg, false); err != nil {
		t.Fatal(err)
	}
	h, err := New(config.NewStore(path), host.Tools())
	if err != nil {
		t.Fatal(err)
	}
	h.transfers.chunk = chunk
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() { h.Close(); srv.Close() })
	return h, srv
}

func startNode(t *testing.T, h *Hub, url, id, token string, exec protocol.Executor) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = node.Run(ctx, config.Node{Version: 1, HubURL: url, DeviceID: id, Token: token}, exec, protocol.Device{ID: id, Hostname: "test", OS: "test", Username: "test-user"})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("node shutdown blocked")
		}
	})
	eventually(t, func() bool { h.mu.RLock(); defer h.mu.RUnlock(); return h.connections[id] != nil })
}

type transferOut struct {
	ID       string `json:"transfer_id"`
	Status   string `json:"status"`
	Size     int64  `json:"size"`
	Written  int64  `json:"bytes_written"`
	SHA256   string `json:"sha256"`
	Verified string `json:"verified"`
	Partial  string `json:"partial_path"`
	Target   struct {
		Path string `json:"path"`
	} `json:"target"`
	Error string `json:"-"`
}

func transferCall(t *testing.T, h *Hub, principal string, args map[string]any) (transferOut, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := h.Transfer(context.Background(), principal, raw)
	if err != nil {
		return transferOut{}, err
	}
	var out transferOut
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	out.Error = res.Error
	return out, nil
}

func randomFile(t *testing.T, path string, n int, mode os.FileMode) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return data
}

func noStaging(t *testing.T, dir string) {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(dir, ".rmc-transfer-*"))
	if len(left) != 0 {
		t.Fatalf("staging files left: %v", left)
	}
}

func TestTransferFileBetweenDevices(t *testing.T) {
	h, srv := transferFixture(t, 64<<10)
	startNode(t, h, srv.URL, "dev1", deviceToken, newRuntime(t))
	startNode(t, h, srv.URL, "dev2", dev2Token, newRuntime(t))
	srcDir, dstDir := t.TempDir(), t.TempDir()
	src := filepath.Join(srcDir, "tool.bin")
	data := randomFile(t, src, 300<<10+7, 0750)

	// Ending target_path with / keeps the name; create_parents makes sub/.
	out, err := transferCall(t, h, "alice", map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2", "target_path": dstDir + "/sub/", "create_parents": true})
	if err != nil || out.Error != "" || out.Status != "done" {
		t.Fatalf("transfer: %+v %v", out, err)
	}
	dst := filepath.Join(dstDir, "sub", "tool.bin")
	got, err := os.ReadFile(dst)
	sum := sha256.Sum256(data)
	if err != nil || !bytes.Equal(got, data) || out.SHA256 != hex.EncodeToString(sum[:]) || out.Verified != "sha256" || out.Size != int64(len(data)) || out.Written != out.Size || out.Target.Path != dst {
		t.Fatalf("copy: equal=%v out=%+v err=%v", bytes.Equal(got, data), out, err)
	}
	if info, _ := os.Stat(dst); info.Mode().Perm() != 0750 {
		t.Fatalf("mode %v", info.Mode())
	}
	noStaging(t, filepath.Join(dstDir, "sub"))

	// An existing target is refused before copying, then replaced with overwrite.
	other := randomFile(t, src, 1000, 0644)
	out, err = transferCall(t, h, "alice", map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2", "target_path": dst})
	if err != nil || out.Status != "failed" || !strings.Contains(out.Error, "already exists") || out.Written != 0 {
		t.Fatalf("existing target: %+v %v", out, err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, data) {
		t.Fatal("refused transfer changed the target")
	}
	out, err = transferCall(t, h, "alice", map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2", "target_path": dst, "overwrite": true, "mode": "0600"})
	if got, _ := os.ReadFile(dst); err != nil || out.Status != "done" || !bytes.Equal(got, other) {
		t.Fatalf("overwrite: %+v %v", out, err)
	}
	if info, _ := os.Stat(dst); info.Mode().Perm() != 0600 {
		t.Fatalf("explicit mode %v", info.Mode())
	}
	// Within one device, and an empty file.
	empty := filepath.Join(srcDir, "empty")
	if err := os.WriteFile(empty, nil, 0644); err != nil {
		t.Fatal(err)
	}
	out, err = transferCall(t, h, "alice", map[string]any{"source_device": "dev1", "source_path": empty, "target_device": "dev1", "target_path": filepath.Join(srcDir, "empty2")})
	if info, statErr := os.Stat(filepath.Join(srcDir, "empty2")); err != nil || out.Status != "done" || statErr != nil || info.Size() != 0 {
		t.Fatalf("empty: %+v %v %v", out, err, statErr)
	}
	// A directory source or target fails cleanly.
	out, _ = transferCall(t, h, "alice", map[string]any{"source_device": "dev1", "source_path": srcDir, "target_device": "dev2", "target_path": filepath.Join(dstDir, "x")})
	if out.Status != "failed" || !strings.Contains(out.Error, "not a regular file") {
		t.Fatalf("directory source: %+v", out)
	}
	out, _ = transferCall(t, h, "alice", map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2", "target_path": dstDir, "overwrite": true})
	if out.Status != "failed" || !strings.Contains(out.Error, "is a directory") {
		t.Fatalf("directory target: %+v", out)
	}
	noStaging(t, dstDir)
}

// Files over 16 MiB are verified by size rather than sha256.
func TestTransferFileLarge(t *testing.T) {
	h, srv := transferFixture(t, 1<<20)
	startNode(t, h, srv.URL, "dev1", deviceToken, newRuntime(t))
	startNode(t, h, srv.URL, "dev2", dev2Token, newRuntime(t))
	src := filepath.Join(t.TempDir(), "big")
	data := randomFile(t, src, 17<<20+3, 0644)
	dst := filepath.Join(t.TempDir(), "big")
	out, err := transferCall(t, h, "alice", map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2", "target_path": dst, "wait_seconds": 40})
	if err != nil || out.Status != "done" || out.Verified != "size" {
		t.Fatalf("large: %+v %v", out, err)
	}
	got, _ := os.ReadFile(dst)
	sum := sha256.Sum256(data)
	if !bytes.Equal(got, data) || out.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("large copy differs")
	}
}

func TestTransferFilePollCancelAndSourceChange(t *testing.T) {
	h, srv := transferFixture(t, 16<<10)
	var mu sync.Mutex
	var sourceHook, targetHook func(tool string, args json.RawMessage) error
	startNode(t, h, srv.URL, "dev1", deviceToken, &hookedRuntime{Runtime: newRuntime(t), hook: func(tool string, args json.RawMessage) error {
		mu.Lock()
		hook := sourceHook
		mu.Unlock()
		if hook != nil {
			return hook(tool, args)
		}
		return nil
	}})
	startNode(t, h, srv.URL, "dev2", dev2Token, &hookedRuntime{Runtime: newRuntime(t), hook: func(tool string, args json.RawMessage) error {
		mu.Lock()
		hook := targetHook
		mu.Unlock()
		if hook != nil {
			return hook(tool, args)
		}
		return nil
	}})
	src := filepath.Join(t.TempDir(), "src")
	data := randomFile(t, src, 200<<10, 0644)
	dstDir := t.TempDir()

	// A slow transfer returns a transfer_id, keeps the Hub busy and can be
	// waited for; only its own principal can see it.
	mu.Lock()
	targetHook = func(tool string, _ json.RawMessage) error {
		if tool == "write_file" {
			time.Sleep(20 * time.Millisecond)
		}
		return nil
	}
	mu.Unlock()
	out, err := transferCall(t, h, "alice", map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2", "target_path": filepath.Join(dstDir, "slow"), "wait_seconds": 0})
	if err != nil || out.Status != "running" || out.ID == "" || out.Error != "" {
		t.Fatalf("start: %+v %v", out, err)
	}
	if h.Idle() {
		t.Fatal("Hub idle during a transfer")
	}
	if _, err := transferCall(t, h, "bob", map[string]any{"transfer_id": out.ID}); err == nil || !strings.Contains(err.Error(), "unknown transfer_id") {
		t.Fatalf("other principal saw the transfer: %v", err)
	}
	for out.Status == "running" {
		if out, err = transferCall(t, h, "alice", map[string]any{"transfer_id": out.ID, "wait_seconds": 5}); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(dstDir, "slow")); out.Status != "done" || !bytes.Equal(got, data) {
		t.Fatalf("polled transfer: %+v", out)
	}
	eventually(t, h.Idle)

	// Cancel while a chunk is in flight: the staging file is removed.
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	mu.Lock()
	targetHook = func(tool string, args json.RawMessage) error {
		if tool == "write_file" && bytes.Contains(args, []byte(`"append":true`)) {
			select {
			case <-release:
			case <-time.After(10 * time.Second):
			}
			return errors.New("released")
		}
		return nil
	}
	mu.Unlock()
	out, _ = transferCall(t, h, "alice", map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2", "target_path": filepath.Join(dstDir, "canceled"), "wait_seconds": 0})
	second, _ := transferCall(t, h, "alice", map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2", "target_path": filepath.Join(dstDir, "canceled2"), "wait_seconds": 0})
	eventually(t, func() bool { left, _ := filepath.Glob(filepath.Join(dstDir, ".rmc-transfer-*")); return len(left) == 2 })
	// A client runs at most two transfers at once.
	if _, err := transferCall(t, h, "alice", map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2", "target_path": filepath.Join(dstDir, "third")}); err == nil || !strings.Contains(err.Error(), "already runs 2") {
		t.Fatalf("per-client limit: %v", err)
	}
	out, err = transferCall(t, h, "alice", map[string]any{"transfer_id": out.ID, "cancel": true, "wait_seconds": 10})
	if err != nil || out.Status != "canceled" || !strings.Contains(out.Error, "canceled") || out.Partial != "" {
		t.Fatalf("cancel: %+v %v", out, err)
	}
	if second, err = transferCall(t, h, "alice", map[string]any{"transfer_id": second.ID, "cancel": true, "wait_seconds": 10}); err != nil || second.Status != "canceled" {
		t.Fatalf("cancel second: %+v %v", second, err)
	}
	once.Do(func() { close(release) })
	if _, err := os.Stat(filepath.Join(dstDir, "canceled")); !os.IsNotExist(err) {
		t.Fatal("canceled transfer installed its target")
	}
	noStaging(t, dstDir)

	// A source that changes during the transfer is never installed.
	mu.Lock()
	targetHook = nil
	sourceHook = func(tool string, args json.RawMessage) error {
		if tool == "read" && bytes.Contains(args, []byte(`"offset":16384`)) {
			f, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0)
			if err == nil {
				_, _ = f.Write([]byte("more"))
				_ = f.Close()
			}
		}
		return nil
	}
	mu.Unlock()
	out, _ = transferCall(t, h, "alice", map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2", "target_path": filepath.Join(dstDir, "changed")})
	if out.Status != "failed" || !strings.Contains(out.Error, "changed during the transfer") {
		t.Fatalf("changed source: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "changed")); !os.IsNotExist(err) {
		t.Fatal("changed source was installed")
	}
	noStaging(t, dstDir)
}

func TestTransferFileRefusals(t *testing.T) {
	h, srv := transferFixture(t, 64<<10)
	startNode(t, h, srv.URL, "dev1", deviceToken, newRuntime(t))
	startNode(t, h, srv.URL, "dev2", dev2Token, oldRuntime{newRuntime(t)})
	src := filepath.Join(t.TempDir(), "f")
	randomFile(t, src, 10, 0644)
	dst := filepath.Join(t.TempDir(), "f")
	for _, tc := range []struct {
		principal string
		args      map[string]any
		want      string
	}{
		{"alice", map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2", "target_path": dst}, "does not support the move_path tool"},
		{"bob", map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2", "target_path": dst}, `not authorized for device "dev1"`},
		{"alice", map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev3", "target_path": dst}, `not authorized for device "dev3"`},
		{"alice", map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2"}, "target_path is required"},
		{"alice", map[string]any{"source_device": "dev1", "source_path": src + "\n", "target_device": "dev2", "target_path": dst}, "line breaks"},
		{"alice", map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2", "target_path": dst, "mode": "999"}, "octal"},
		{"alice", map[string]any{"source_device": "dev1", "source_path": "/", "target_device": "dev2", "target_path": "/tmp/"}, "no file name"},
		{"alice", map[string]any{"transfer_id": "abc", "source_path": src}, "give only transfer_id"},
		{"alice", map[string]any{"cancel": true}, "needs the transfer_id"},
		{"alice", map[string]any{"transfer_id": "abc"}, "unknown transfer_id"},
		{"alice", map[string]any{"wait_seconds": 41, "transfer_id": "abc"}, "wait_seconds"},
		{"alice", map[string]any{"bogus": 1}, "unknown field"},
	} {
		if _, err := transferCall(t, h, tc.principal, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: %v, want %q", tc.args, err, tc.want)
		}
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("refused transfer wrote the target")
	}
}

// transfer_file is a Hub tool on the MCP surface and copies end to end.
func TestTransferFileOverMCP(t *testing.T) {
	h, srv := transferFixture(t, 64<<10)
	startNode(t, h, srv.URL, "dev1", deviceToken, newRuntime(t))
	startNode(t, h, srv.URL, "dev2", dev2Token, newRuntime(t))
	src := filepath.Join(t.TempDir(), "f")
	data := randomFile(t, src, 100<<10, 0644)
	dst := filepath.Join(t.TempDir(), "f")
	session := mcpClient(t, srv.URL, aliceToken, "2025-11-25")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "transfer_file" {
			raw, _ := json.Marshal(tool.InputSchema)
			found = !bytes.Contains(raw, []byte(`"device"`))
		}
	}
	if !found {
		t.Fatal("transfer_file is not listed as a Hub tool")
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "transfer_file", Arguments: map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2", "target_path": dst}})
	if err != nil || res.IsError {
		t.Fatalf("call: %+v %v", res, err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, data) {
		t.Fatal("MCP transfer differs")
	}
	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "transfer_file", Arguments: map[string]any{"source_device": "dev1", "source_path": src, "target_device": "dev2", "target_path": dst}})
	if err != nil || !res.IsError {
		t.Fatalf("failed transfer is not a tool error: %+v %v", res, err)
	}
}
