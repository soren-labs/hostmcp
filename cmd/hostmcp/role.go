package main

import (
	"fmt"
	"strings"
)

func checkRole(role, command string) error {
	if role != "all" && role != "hub" && role != "agent" {
		return fmt.Errorf("invalid build component %q", role)
	}
	if role == "all" {
		return nil
	}
	switch command {
	case "help", "--help", "-h", "version", "--version", "service", "doctor":
		return nil
	case "hub", "device", "token", "connect", "call":
		if role == "hub" {
			return nil
		}
	case "agent":
		if role == "agent" {
			return nil
		}
	}
	return fmt.Errorf("command %q is unavailable in hostmcp-%s; use the matching component or hostmcp compatibility binary", command, role)
}

func roleUsage(role string) string {
	if role == "all" {
		return usage
	}
	lines := strings.Split(usage, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "hostmcp" {
			if checkRole(role, fields[1]) != nil {
				continue
			}
			line = strings.Replace(line, "hostmcp ", "hostmcp-"+role+" ", 1)
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}
