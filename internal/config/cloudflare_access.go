package config

import (
	"errors"
	"net/mail"
	"regexp"
	"strings"
	"unicode"
)

// CloudflareAccess trusts Access application assertions, not OAuth access tokens.
type CloudflareAccess struct {
	TeamDomain string                      `json:"team_domain"`
	Audience   string                      `json:"audience"`
	Principals []CloudflareAccessPrincipal `json:"principals"`
}

type CloudflareAccessPrincipal struct {
	Email   string   `json:"email"`
	Devices []string `json:"devices"`
}

var accessTeamDomain = regexp.MustCompile(`^https://[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.cloudflareaccess\.com$`)

// AccessEmail normalizes case only; display names, wildcards and whitespace are
// not identity mappings. Errors deliberately omit the supplied identity.
func AccessEmail(raw string) (string, error) {
	email := strings.ToLower(raw)
	if email == "" || len(email) > 254 || strings.Contains(email, "*") || strings.ContainsFunc(email, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return "", errors.New("Access requires a specific email address")
	}
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || address.Name != "" {
		return "", errors.New("Access requires a specific email address")
	}
	return email, nil
}

func (a *CloudflareAccess) Validate() error {
	if a == nil {
		return nil
	}
	if !accessTeamDomain.MatchString(a.TeamDomain) {
		return errors.New("Access team_domain must be https://TEAM.cloudflareaccess.com without port or path")
	}
	if a.Audience == "" || len(a.Audience) > 512 || strings.TrimSpace(a.Audience) != a.Audience || strings.ContainsFunc(a.Audience, unicode.IsControl) {
		return errors.New("Access requires a nonblank exact application audience")
	}
	if len(a.Principals) > 256 {
		return errors.New("Access permits at most 256 principals")
	}
	seen := map[string]bool{}
	for _, p := range a.Principals {
		email, err := AccessEmail(p.Email)
		if err != nil {
			return err
		}
		if seen[email] || len(p.Devices) == 0 || len(p.Devices) > 1024 {
			return errors.New("Access requires unique emails with explicit nodes")
		}
		seen[email] = true
		for _, id := range p.Devices {
			if !ValidID(id) {
				return errors.New("Access node grants must be explicit IDs; wildcard is forbidden")
			}
		}
	}
	return nil
}
