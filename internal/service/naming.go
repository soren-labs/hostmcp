package service

import (
	"path/filepath"
	"strings"

	"github.com/soren-labs/rmc/internal/config"
)

// Services installed before the project was renamed from HostMCP keep their
// original unit names, launchd labels, log directories and file markers. RMC
// still recognises them so existing services can be removed and reinstalled
// under the same name without a manual cleanup step.
const (
	namePrefix   = "rmc-"
	legacyPrefix = "hostmcp-"
	legacyMarker = "Managed by HostMCP"
)

func validName(name string) bool {
	return config.ValidID(name) && (strings.HasPrefix(name, namePrefix) || strings.HasPrefix(name, legacyPrefix))
}

func legacyName(name string) bool {
	return strings.HasPrefix(name, legacyPrefix)
}

// launchdLabel returns the launchd label for a service name.
func launchdLabel(name string) string {
	if legacyName(name) {
		return "com.hostmcp." + name
	}
	return "com.rmc." + name
}

// logDirName returns the directory under Library/Logs used by a service.
func logDirName(name string) string {
	if legacyName(name) {
		return "HostMCP"
	}
	return "RMC"
}

// generated reports whether a unit or plist was written by RMC or by
// HostMCP before the rename.
func generated(body string) bool {
	return strings.Contains(body, marker) || strings.Contains(body, legacyMarker)
}

func userLogDir(home, name string) string {
	return filepath.Join(home, "Library", "Logs", logDirName(name))
}
