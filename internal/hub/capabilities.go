package hub

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/soren-labs/rmc/internal/protocol"
)

// legacyToolArgs is the native tool surface of wire-v1 nodes (formerly Agents) that predate the
// optional hello "tool_args" advertisement (HostMCP and RMC up to v0.2.4).
// It is frozen: never add to it. Such nodes decode tool arguments strictly,
// so a newer argument would fail there with a generic "unknown field" error,
// and a newer tool name would fail as unknown.
var legacyToolArgs = toolSetOf(map[string][]string{
	"device_info":       {},
	"exec_command":      {"cmd", "cwd", "env", "login", "max_output_bytes", "request_id", "shell", "timeout_seconds", "tty", "yield_time_ms"},
	"write_stdin":       {"chars", "close_stdin", "cols", "max_output_bytes", "rows", "session_id", "signal", "yield_time_ms"},
	"list_processes":    {},
	"terminate_process": {"session_id", "signal"},
	"read":              {"cwd", "encoding", "end_line", "max_bytes", "paths", "start_line"},
	"write_file":        {"content", "create_parents", "cwd", "encoding", "mode", "overwrite", "path"},
	"apply_patch":       {"cwd", "patch"},
	"view_image":        {"cwd", "path"},
})

// postV1Tools names native tools added after legacyToolArgs was frozen. A Hub
// never dispatches them to a node that did not advertise them. New argument
// names on legacy tools need no entry: they are detected from the frozen table.
var postV1Tools = map[string]bool{"search": true}

const (
	maxAdvertisedTools = 128
	maxAdvertisedArgs  = 64
	maxAdvertisedName  = 64
)

type toolSet map[string]map[string]bool

func toolSetOf(m map[string][]string) toolSet {
	set := make(toolSet, len(m))
	for tool, args := range m {
		set[tool] = make(map[string]bool, len(args))
		for _, a := range args {
			set[tool][a] = true
		}
	}
	return set
}

// advertisedTools validates a node's hello advertisement. A missing,
// malformed or oversized advertisement yields nil, which means "legacy node".
func advertisedTools(m map[string][]string) toolSet {
	if len(m) == 0 || len(m) > maxAdvertisedTools {
		return nil
	}
	for tool, args := range m {
		if tool == "" || len(tool) > maxAdvertisedName || len(args) > maxAdvertisedArgs {
			return nil
		}
		for _, a := range args {
			if a == "" || len(a) > maxAdvertisedName {
				return nil
			}
		}
	}
	return toolSetOf(m)
}

// unsupportedFeature names a Hub-known native tool or argument that the target
// node does not support, or returns "" when the call may be dispatched. Tools
// and arguments unknown to this Hub are left for the node to accept or reject.
func (h *Hub) unsupportedFeature(d *deviceConn, tool string, args json.RawMessage) string {
	known, ok := h.toolArgs[tool]
	if !ok {
		return ""
	}
	nodeTools := d.tools
	legacy := nodeTools == nil
	if legacy {
		if postV1Tools[tool] {
			return fmt.Sprintf("the %s tool", tool)
		}
		if _, native := legacyToolArgs[tool]; !native {
			return ""
		}
		nodeTools = legacyToolArgs
	}
	have, ok := nodeTools[tool]
	if !ok {
		return fmt.Sprintf("the %s tool", tool)
	}
	var fields map[string]json.RawMessage
	if len(args) == 0 || json.Unmarshal(args, &fields) != nil {
		return ""
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if known[name] && !have[name] {
			return fmt.Sprintf("the %s argument of %s", name, tool)
		}
	}
	return ""
}

func nodeVersion(d *deviceConn) string {
	if d.info.Version == "" {
		return "an older version"
	}
	return d.info.Version
}

func toolArgsOf(tools []protocol.Tool) toolSet {
	return toolSetOf(protocol.ToolArgNames(tools))
}
