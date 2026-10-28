// Package protocol defines HostMCP's private hub-to-device protocol.
package protocol

import (
	"context"
	"encoding/json"
	"time"
)

const Version = "0.2.1"
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
	ID           string    `json:"id"`
	Hostname     string    `json:"hostname"`
	OS           string    `json:"os"`
	Arch         string    `json:"arch"`
	Username     string    `json:"username"`
	UID          string    `json:"uid"`
	EUID         string    `json:"euid,omitempty"`
	ExpectedUser string    `json:"expected_user,omitempty"`
	Home         string    `json:"home"`
	Version      string    `json:"version"`
	Online       bool      `json:"online"`
	ConnectedAt  time.Time `json:"connected_at,omitzero"`
}

type Message struct {
	Type      string          `json:"type"`
	Version   int             `json:"version,omitempty"`
	ID        string          `json:"id,omitempty"`
	Principal string          `json:"principal,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	Device    *Device         `json:"device,omitempty"`
	Result    *Result         `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
}

type Executor interface {
	Call(context.Context, string, string, json.RawMessage) (Result, error)
	Close() error
}
