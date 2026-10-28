// Package mcpserver maps the native tool API onto the official MCP SDK.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/soren-labs/hostmcp/internal/protocol"
)

type Options struct {
	Tools       []protocol.Tool
	Remote      bool
	Principal   string
	Call        func(context.Context, string, string, string, json.RawMessage) (protocol.Result, error)
	ListDevices func(context.Context, string) ([]protocol.Device, error)
}

func New(opts Options) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "hostmcp", Version: protocol.Version}, &mcp.ServerOptions{
		Instructions: "Each HostMCP node is one machine plus the OS account running its agent. Different node IDs on the same machine can represent ordinary and root accounts. A credential grants the native authority of its allowed nodes; there are no directory or command allowlists. Select an online node and check its username and UID before remote tool calls. Long commands return a session ID; use write_stdin to collect output and terminate_process to stop them. A network failure does not roll back a command: query processes after reconnecting and do not blindly repeat side effects.",
	})
	principal := func(req *mcp.CallToolRequest) (string, error) {
		if req.Extra != nil && req.Extra.TokenInfo != nil && req.Extra.TokenInfo.UserID != "" {
			return req.Extra.TokenInfo.UserID, nil
		}
		if opts.Remote {
			return "", errors.New("authenticated principal is missing")
		}
		if opts.Principal != "" {
			return opts.Principal, nil
		}
		return "local", nil
	}
	for _, spec := range opts.Tools {
		spec := spec
		schemaBytes, err := json.Marshal(spec.InputSchema)
		if err != nil {
			panic(fmt.Errorf("tool schema %s: %w", spec.Name, err))
		}
		var schema map[string]any
		if err := json.Unmarshal(schemaBytes, &schema); err != nil {
			panic(err)
		}
		if opts.Remote {
			properties, ok := schema["properties"].(map[string]any)
			if !ok {
				properties = map[string]any{}
				schema["properties"] = properties
			}
			properties["device"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 64, "description": "Registered account-node ID from list_devices. Check username and UID; two nodes on the same machine may have different account privileges."}
			required, _ := schema["required"].([]any)
			schema["required"] = append(required, "device")
		}
		mcp.AddTool[map[string]any, any](server, &mcp.Tool{
			Name: spec.Name, Description: spec.Description, InputSchema: schema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: spec.ReadOnly},
		}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			p, err := principal(req)
			if err != nil {
				return nil, nil, err
			}
			device := ""
			if opts.Remote {
				device, _ = args["device"].(string)
				if device == "" {
					return nil, nil, errors.New("device is required")
				}
				delete(args, "device")
			}
			raw, err := json.Marshal(args)
			if err != nil {
				return nil, nil, err
			}
			if len(raw) > protocol.MaxMessageBytes-4096 {
				return nil, nil, errors.New("tool arguments are too large")
			}
			if opts.Call == nil {
				return nil, nil, errors.New("tool executor is unavailable")
			}
			result, err := opts.Call(ctx, p, device, spec.Name, raw)
			if err != nil {
				return nil, nil, err
			}
			return Convert(result), nil, nil
		})
	}
	if opts.ListDevices != nil {
		mcp.AddTool[map[string]any, any](server, &mcp.Tool{
			Name: "list_devices", Description: "List registered devices visible to this client, their online state, and the OS identity used for execution.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, func(ctx context.Context, req *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			p, err := principal(req)
			if err != nil {
				return nil, nil, err
			}
			devices, err := opts.ListDevices(ctx, p)
			if err != nil {
				return nil, nil, err
			}
			if devices == nil {
				devices = []protocol.Device{}
			}
			return Convert(protocol.JSONResult(map[string]any{"devices": devices})), nil, nil
		})
	}
	return server
}

// Convert preserves both human-readable and structured results. OS/tool errors
// are MCP tool errors, rather than JSON-RPC transport failures.
func Convert(result protocol.Result) *mcp.CallToolResult {
	out := &mcp.CallToolResult{Content: []mcp.Content{}}
	if result.Error != "" {
		out.IsError = true
		out.Content = append(out.Content, &mcp.TextContent{Text: result.Error})
	}
	if len(result.Data) > 0 {
		if !json.Valid(result.Data) {
			out.IsError = true
			out.Content = append(out.Content, &mcp.TextContent{Text: "device returned invalid JSON"})
			return out
		}
		out.StructuredContent = result.Data
		out.Content = append(out.Content, &mcp.TextContent{Text: string(result.Data)})
	}
	if result.Image != nil {
		out.Content = append(out.Content, &mcp.ImageContent{MIMEType: result.Image.MIMEType, Data: result.Image.Data})
	}
	if len(out.Content) == 0 {
		out.Content = append(out.Content, &mcp.TextContent{Text: "{}"})
	}
	return out
}
