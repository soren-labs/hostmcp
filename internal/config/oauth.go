package config

import (
	"errors"
	"net/url"
	"strings"
)

// OAuth trusts one external issuer. Scope claims never grant node access.
type OAuth struct {
	Issuer     string           `json:"issuer"`
	Scopes     []string         `json:"scopes"`
	Principals []OAuthPrincipal `json:"principals"`
}
type OAuthPrincipal struct {
	Subject string   `json:"subject"`
	Devices []string `json:"devices"`
}

// OAuthURL requires authenticated HTTPS transport except for isolated loopback tests.
func OAuthURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("OAuth URL must be absolute without credentials, query or fragment")
	}
	origin := u.Scheme + "://" + u.Host
	_, err = CheckURL(origin, false)
	return err
}
func (o *OAuth) Validate(publicURL string) error {
	if o == nil {
		return nil
	}
	if err := OAuthURL(publicURL); err != nil {
		return err
	}
	if err := OAuthURL(o.Issuer); err != nil {
		return err
	}
	if len(o.Scopes) == 0 || len(o.Scopes) > 32 || len(o.Principals) > 256 {
		return errors.New("OAuth requires 1-32 scopes and at most 256 principals")
	}
	for _, s := range o.Scopes {
		if s == "" || len(s) > 128 || s == "offline_access" {
			return errors.New("invalid OAuth resource scope")
		}
		for _, c := range s {
			if c < 0x21 || c > 0x7e || c == '"' || c == '\\' {
				return errors.New("invalid OAuth scope character")
			}
		}
	}
	seen := map[string]bool{}
	for _, p := range o.Principals {
		if p.Subject == "" || len(p.Subject) > 512 || seen[p.Subject] || len(p.Devices) == 0 || len(p.Devices) > 1024 {
			return errors.New("OAuth requires unique nonempty subjects with explicit nodes")
		}
		if strings.ContainsAny(p.Subject, "\r\n\x00") {
			return errors.New("invalid OAuth subject")
		}
		seen[p.Subject] = true
		for _, d := range p.Devices {
			if !ValidID(d) {
				return errors.New("OAuth node grants must be explicit IDs; wildcard is forbidden")
			}
		}
	}
	return nil
}
