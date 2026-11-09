package main

import (
	"fmt"
	"strings"
)

// normalizeRole maps the pre-v0.3 "agent" component name to "node".
func normalizeRole(role string) string {
	if role == "agent" {
		return "node"
	}
	return role
}

func checkRole(role, command string) error {
	role = normalizeRole(role)
	if role != "all" && role != "hub" && role != "node" {
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
	case "node", "agent":
		if role == "node" {
			return nil
		}
	}
	return fmt.Errorf("command %q is unavailable in rmc-%s; use the matching component or rmc compatibility binary", command, role)
}

func roleUsage(role string) string {
	role = normalizeRole(role)
	if role == "all" {
		return usage
	}
	lines := strings.Split(usage, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "rmc" {
			if checkRole(role, fields[1]) != nil {
				continue
			}
			line = strings.Replace(line, "rmc ", "rmc-"+role+" ", 1)
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}
