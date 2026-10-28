package host

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/soren-labs/hostmcp/internal/protocol"
)

func TestDeviceInfoUsesRegisteredIdentity(t *testing.T) {
	r, err := NewForDevice("workstation-worker")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	result, err := r.Call(context.Background(), "client", "device_info", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Device protocol.Device `json:"device"`
		EUID   int             `json:"euid"`
	}
	if err := json.Unmarshal(result.Data, &info); err != nil {
		t.Fatal(err)
	}
	if info.Device.ID != "workstation-worker" || info.EUID != os.Geteuid() || info.Device.EUID != strconv.Itoa(os.Geteuid()) {
		t.Fatalf("device identity does not describe this executor: %+v", info)
	}
	var raw struct {
		Device map[string]any `json:"device"`
	}
	if err := json.Unmarshal(result.Data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw.Device["connected_at"]; present {
		t.Fatal("local execution metadata must omit unknown Hub connection time")
	}
}

func TestLocalIdentityAndKnownConnectionTime(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := r.Info("").ID; got != "local" {
		t.Fatalf("default runtime ID = %q", got)
	}
	connected := r.Info("registered-node")
	connected.ConnectedAt = time.Date(2026, time.October, 28, 9, 0, 0, 0, time.UTC)
	b, err := json.Marshal(connected)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["connected_at"] != "2026-10-28T09:00:00Z" || raw["id"] != "registered-node" {
		t.Fatalf("known connection metadata missing: %s", b)
	}
}
