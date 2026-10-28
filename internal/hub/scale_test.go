package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/soren-labs/hostmcp/internal/client"
	"github.com/soren-labs/hostmcp/internal/config"
	"github.com/soren-labs/hostmcp/internal/protocol"
)

// TestScaleAccountNodes is opt-in: normal CI does not start hundreds of sockets.
// It launches a separate, temporary Hub, never an existing deployment. Nodes are
// simulated protocol peers, not real hosts or native OS-account executors.
//
//	HOSTMCP_SCALE_BINARY="$PWD/build/hostmcp" go test ./internal/hub \
//	  -run '^TestScaleAccountNodes$' -count=1 -v -timeout=2m
func TestScaleAccountNodes(t *testing.T) {
	binary := os.Getenv("HOSTMCP_SCALE_BINARY")
	if binary == "" {
		t.Skip("set HOSTMCP_SCALE_BINARY to an existing hostmcp executable for 200-node verification")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	// The production Hub intentionally bounds active requests/handshakes at 8.
	// Online inventory size is a separate dimension from simultaneous requests.
	const nodeCount, concurrency, reconnectCount = 200, 8, 20
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	url := "http://" + address
	state := filepath.Join(t.TempDir(), "hub.json")
	clientToken, err := config.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Hub{Version: config.FormatVersion, Listen: address, PublicURL: url,
		Clients: []config.Client{{ID: "scale-client", TokenHash: config.Hash(clientToken), Devices: []string{"*"}}}}
	tokens := make([]string, nodeCount)
	ids := make([]string, nodeCount)
	for i := range ids {
		ids[i] = fmt.Sprintf("simulated-%03d", i)
		tokens[i], err = config.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		cfg.Devices = append(cfg.Devices, config.Device{ID: ids[i], TokenHash: config.Hash(tokens[i])})
	}
	if err := config.WriteJSON(state, cfg, false); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	command := exec.CommandContext(ctx, binary, "hub", "serve", "--config", state)
	command.Stdout, command.Stderr = &logs, &logs
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	processDone := make(chan error, 1)
	go func() { processDone <- command.Wait() }()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		select {
		case <-processDone:
		case <-time.After(5 * time.Second):
			t.Error("temporary Hub did not stop")
		}
	})
	started := time.Now()
	healthClient := &http.Client{Timeout: 500 * time.Millisecond}
	for {
		response, healthErr := healthClient.Get(url + "/healthz")
		if healthErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Since(started) > 10*time.Second {
			t.Fatal("isolated Hub did not become healthy")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopSamples, samplesDone := sampleHubResources(command.Process.Pid)
	defer stopSamples()
	connections := make([]*websocket.Conn, nodeCount)
	var peers sync.WaitGroup
	t.Cleanup(func() {
		for _, connection := range connections {
			if connection != nil {
				_ = connection.CloseNow()
			}
		}
		cancel()
		peers.Wait()
	})
	connect := func(i int) error {
		connection, _, dialErr := websocket.Dial(ctx, strings.Replace(url, "http:", "ws:", 1)+"/agent/"+ids[i],
			&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + tokens[i]}}})
		if dialErr != nil {
			return dialErr
		}
		connection.SetReadLimit(protocol.MaxMessageBytes)
		info := protocol.Device{ID: ids[i], Hostname: ids[i], OS: "simulation", Arch: "synthetic", Username: "scale-user", UID: "1001", Version: protocol.Version}
		if err := wsjson.Write(ctx, connection, protocol.Message{Type: "hello", Version: protocol.WireVersion, Device: &info}); err != nil {
			_ = connection.CloseNow()
			return err
		}
		var welcome protocol.Message
		if err := wsjson.Read(ctx, connection, &welcome); err != nil || welcome.Type != "welcome" {
			_ = connection.CloseNow()
			return fmt.Errorf("simulated node handshake failed: %v", err)
		}
		connections[i] = connection
		peers.Add(1)
		go func() {
			defer peers.Done()
			for {
				var request protocol.Message
				if err := wsjson.Read(ctx, connection, &request); err != nil {
					return
				}
				if request.Type != "request" {
					continue
				}
				result := protocol.JSONResult(map[string]any{"node_id": ids[i], "principal": request.Principal, "simulated": true})
				if err := wsjson.Write(ctx, connection, protocol.Message{Type: "result", ID: request.ID, Result: &result}); err != nil {
					return
				}
			}
		}()
		return nil
	}
	parallelScale(t, nodeCount, concurrency, connect)
	session, err := client.Connect(ctx, url, clientToken, "2026-07-28", false)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	verifyInventory := func() {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_devices", Arguments: map[string]any{}})
		if err != nil || result.IsError {
			t.Fatalf("inventory request failed: %v", err)
		}
		data, _ := json.Marshal(result.StructuredContent)
		var inventory struct {
			Devices []protocol.Device `json:"devices"`
		}
		if err := json.Unmarshal(data, &inventory); err != nil || len(inventory.Devices) != nodeCount {
			t.Fatalf("expected %d simulated nodes, got %d (%v)", nodeCount, len(inventory.Devices), err)
		}
		for _, device := range inventory.Devices {
			if !device.Online {
				t.Fatalf("node is offline: %s", device.ID)
			}
		}
	}
	verifyInventory()
	// Give the separate process a short settled-online sampling window. This is
	// a bounded functional/load check, not a long-running endurance benchmark.
	time.Sleep(2 * time.Second)
	route := func(i int) error {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "device_info", Arguments: map[string]any{"device": ids[i]}})
		if err != nil {
			return err
		}
		if result.IsError {
			return fmt.Errorf("MCP tool error for simulated node %s", ids[i])
		}
		data, _ := json.Marshal(result.StructuredContent)
		var reply struct {
			NodeID    string `json:"node_id"`
			Principal string `json:"principal"`
			Simulated bool   `json:"simulated"`
		}
		if err := json.Unmarshal(data, &reply); err != nil {
			return err
		}
		if reply.NodeID != ids[i] || reply.Principal != "scale-client" || !reply.Simulated {
			return fmt.Errorf("incorrect route or principal for %s", ids[i])
		}
		return nil
	}
	parallelScale(t, nodeCount, concurrency, route)
	for i := 0; i < reconnectCount; i++ {
		_ = connections[i].CloseNow()
	}
	parallelScale(t, reconnectCount, concurrency, connect)
	verifyInventory()
	parallelScale(t, reconnectCount, concurrency, route)
	stopSamples()
	report := map[string]any{"kind": "simulated_account_nodes", "real_hosts": false,
		"nodes": nodeCount, "successful_routes": nodeCount, "route_concurrency": concurrency,
		"reconnected_nodes": reconnectCount, "post_reconnect_routes": reconnectCount,
		"seconds": time.Since(started).Seconds(), "hub_resources": <-samplesDone}
	encoded, _ := json.Marshal(report)
	t.Logf("HOSTMCP_SCALE_REPORT %s", encoded)
}

func parallelScale(t *testing.T, count, concurrency int, action func(int) error) {
	t.Helper()
	jobs := make(chan int)
	errors := make(chan error, count)
	var workers sync.WaitGroup
	for n := 0; n < concurrency; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				if err := action(i); err != nil {
					errors <- err
				}
			}
		}()
	}
	for i := 0; i < count; i++ {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
}

// Samples only the separate Hub process, not the simulated peers/test process.
// Linux /proc measurements are observed samples, not a guaranteed memory bound.
func sampleHubResources(pid int) (func(), <-chan map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan map[string]any, 1)
	go func() {
		if runtime.GOOS != "linux" {
			<-ctx.Done()
			done <- map[string]any{"available": false, "reason": "Linux /proc sampling only"}
			return
		}
		hzOutput, _ := exec.Command("getconf", "CLK_TCK").Output()
		hz, _ := strconv.ParseFloat(strings.TrimSpace(string(hzOutput)), 64)
		var peakRSS, firstTicks, lastTicks uint64
		var first, last time.Time
		samples := 0
		sample := func() {
			status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
			if err != nil {
				return
			}
			for _, line := range strings.Split(string(status), "\n") {
				if strings.HasPrefix(line, "VmRSS:") {
					var rss uint64
					_, _ = fmt.Sscanf(line, "VmRSS: %d kB", &rss)
					if rss > peakRSS {
						peakRSS = rss
					}
				}
			}
			stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if err != nil {
				return
			}
			text := string(stat)
			fields := strings.Fields(text[strings.LastIndex(text, ")")+1:])
			if len(fields) < 13 {
				return
			}
			user, _ := strconv.ParseUint(fields[11], 10, 64)
			system, _ := strconv.ParseUint(fields[12], 10, 64)
			lastTicks, last = user+system, time.Now()
			if samples == 0 {
				firstTicks, first = lastTicks, last
			}
			samples++
		}
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		sample()
		for {
			select {
			case <-ticker.C:
				sample()
			case <-ctx.Done():
				sample()
				report := map[string]any{"available": samples > 0, "samples": samples, "rss_peak_kib": peakRSS}
				if hz > 0 && last.After(first) {
					cpuSeconds := float64(lastTicks-firstTicks) / hz
					report["cpu_seconds"] = cpuSeconds
					report["cpu_percent_one_core"] = 100 * cpuSeconds / last.Sub(first).Seconds()
				}
				done <- report
				return
			}
		}
	}()
	return cancel, done
}
