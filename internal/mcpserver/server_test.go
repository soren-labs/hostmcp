package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/soren-labs/roost/internal/protocol"
)

// Text-only clients must receive the same complete result as clients using
// structuredContent. A summary or an empty content list would lose the command
// output and session ID for clients that ignore the newer structured field.
func TestToolResultPreservesStructuredAndTextOnlyClients(t *testing.T) {
	data := json.RawMessage(`{"session_id":"test-session","output":"你好\nsecond line","running":false,"exit_code":7,"large_integer":9007199254740993}`)
	for _, toolError := range []string{"", "partial execution failed"} {
		t.Run(toolError, func(t *testing.T) {
			wire, err := json.Marshal(Convert(protocol.Result{Data: data, Error: toolError}))
			if err != nil {
				t.Fatal(err)
			}
			var legacy struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			}
			if err := json.Unmarshal(wire, &legacy); err != nil {
				t.Fatal(err)
			}
			wantBlocks := 1
			if toolError != "" {
				wantBlocks++
			}
			if len(legacy.Content) != wantBlocks || legacy.IsError != (toolError != "") {
				t.Fatalf("text-only result lost error or content: %s", wire)
			}
			text := legacy.Content[len(legacy.Content)-1]
			if text.Type != "text" || text.Text != string(data) {
				t.Fatalf("text-only client lost complete JSON data: %s", wire)
			}
			var structured struct {
				Data json.RawMessage `json:"structuredContent"`
			}
			if err := json.Unmarshal(wire, &structured); err != nil {
				t.Fatal(err)
			}
			if string(structured.Data) != string(data) {
				t.Fatalf("structured client lost JSON data or integer precision: %s", wire)
			}
			if toolError != "" && legacy.Content[0].Text != toolError {
				t.Fatalf("tool error text was replaced by JSON data: %s", wire)
			}
		})
	}
}
