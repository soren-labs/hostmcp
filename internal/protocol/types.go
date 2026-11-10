// Package protocol defines RMC's private hub-to-device protocol.
package protocol

import (
	"context"
	"encoding/json"
	"sort"
	"time"
)

var Version = "0.2.4-dev"

const WireVersion = 1
const MaxMessageBytes = 4 << 20

type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	ReadOnly    bool
}

type Image struct {
	MIMEType string `json:"mime_type"`
	Data     []byte `json:"data"`
}

type Result struct {
	Data  json.RawMessage `json:"data,omitempty"`
	Image *Image          `json:"image,omitempty"`
	Error string          `json:"error,omitempty"`
}

func JSONResult(value any) Result {
	b, err := json.Marshal(value)
	if err != nil {
		return Result{Error: err.Error()}
	}
	return Result{Data: b}
}

type Device struct {
	Component      string    `json:"component,omitempty"`
	WireVersion    int       `json:"wire_version,omitempty"`
	LastSeen       time.Time `json:"last_seen,omitzero"`
	DisconnectedAt time.Time `json:"disconnected_at,omitzero"`
	ID             string    `json:"id"`
	Hostname       string    `json:"hostname"`
	OS             string    `json:"os"`
	Arch           string    `json:"arch"`
	Username       string    `json:"username"`
	UID            string    `json:"uid"`
	EUID           string    `json:"euid,omitempty"`
	ExpectedUser   string    `json:"expected_user,omitempty"`
	Home           string    `json:"home"`
	Version        string    `json:"version"`
	Online         bool      `json:"online"`
	ConnectedAt    time.Time `json:"connected_at,omitzero"`
	// AutoUpdate is "stable", "canary", "off" or "manual"; empty from nodes
	// that predate automatic updates. Old Hubs ignore it.
	AutoUpdate string `json:"auto_update,omitempty"`
}

type Message struct {
	Component    string          `json:"component,omitempty"`
	BuildVersion string          `json:"build_version,omitempty"`
	Type         string          `json:"type"`
	Version      int             `json:"version,omitempty"`
	ID           string          `json:"id,omitempty"`
	Principal    string          `json:"principal,omitempty"`
	Tool         string          `json:"tool,omitempty"`
	Args         json.RawMessage `json:"args,omitempty"`
	Device       *Device         `json:"device,omitempty"`
	Result       *Result         `json:"result,omitempty"`
	Error        string          `json:"error,omitempty"`
	// ToolArgs is optional in a node hello: each native tool the node
	// executes and its accepted top-level argument names. Older nodes omit it
	// and older Hubs ignore it.
	ToolArgs map[string][]string `json:"tool_args,omitempty"`
}

// ToolArgNames returns each tool's top-level input property names, sorted.
func ToolArgNames(tools []Tool) map[string][]string {
	out := make(map[string][]string, len(tools))
	for _, t := range tools {
		names := []string{}
		if props, ok := t.InputSchema["properties"].(map[string]any); ok {
			for name := range props {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		out[t.Name] = names
	}
	return out
}

type Executor interface {
	Call(context.Context, string, string, json.RawMessage) (Result, error)
	Close() error
}
