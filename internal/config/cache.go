package config

import (
	"bytes"
	"slices"
)

// Snapshot reuses parsing and validation only when the current private file is
// byte-for-byte identical to the last valid configuration. Reading on every call
// deliberately preserves immediate revocation across processes and in-place
// edits, even when the file size and timestamps are unchanged. There is no TTL,
// metadata-only freshness check, or stale-on-error fallback.
//
// Each caller owns its snapshot. In particular, Update callbacks and callers
// changing nested grants cannot mutate the cached authorization policy.
func (s *Store) Snapshot() (Hub, error) {
	b, err := readConfigFile(s.Path)
	if err != nil {
		return Hub{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cachedBytes != nil && bytes.Equal(b, s.cachedBytes) {
		return cloneHub(s.cached), nil
	}
	var h Hub
	if err := decodeJSON(b, &h); err != nil {
		return Hub{}, err
	}
	if err := validate(h); err != nil {
		return Hub{}, err
	}
	s.cachedBytes, s.cached = b, h
	return cloneHub(h), nil
}

func cloneHub(h Hub) Hub {
	h.AllowedOrigins = slices.Clone(h.AllowedOrigins)
	h.AllowedHosts = slices.Clone(h.AllowedHosts)
	h.Clients = slices.Clone(h.Clients)
	for i := range h.Clients {
		h.Clients[i].Devices = slices.Clone(h.Clients[i].Devices)
	}
	h.Devices = slices.Clone(h.Devices)
	h.Invitations = slices.Clone(h.Invitations)
	if h.OAuth != nil {
		o := *h.OAuth
		o.Scopes = slices.Clone(o.Scopes)
		o.Principals = slices.Clone(o.Principals)
		for i := range o.Principals {
			o.Principals[i].Devices = slices.Clone(o.Principals[i].Devices)
		}
		h.OAuth = &o
	}
	if h.CloudflareAccess != nil {
		a := *h.CloudflareAccess
		a.Principals = slices.Clone(a.Principals)
		for i := range a.Principals {
			a.Principals[i].Devices = slices.Clone(a.Principals[i].Devices)
		}
		h.CloudflareAccess = &a
	}
	return h
}
