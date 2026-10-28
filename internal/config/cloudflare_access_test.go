package config

import (
	"strings"
	"testing"
)

func TestAccessConfiguration(t *testing.T) {
	valid := func() *CloudflareAccess {
		return &CloudflareAccess{TeamDomain: "https://example-team.cloudflareaccess.com", Audience: strings.Repeat("a", 64), Principals: []CloudflareAccessPrincipal{{Email: "Owner@Example.com", Devices: []string{"worker"}}}}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, domain := range []string{"", "http://team.cloudflareaccess.com", "https://cloudflareaccess.com", "https://team.cloudflareaccess.com.evil.example", "https://evilcloudflareaccess.com", "https://team.cloudflareaccess.com:443", "https://team.cloudflareaccess.com/", "https://team.cloudflareaccess.com/path", "https://team.cloudflareaccess.com?x=1", "https://team.cloudflareaccess.com#x", "https://user@team.cloudflareaccess.com", "https://-team.cloudflareaccess.com", "https://team_.cloudflareaccess.com", "https://127.0.0.1"} {
		p := valid()
		p.TeamDomain = domain
		if err := p.Validate(); err == nil {
			t.Fatal("invalid team domain accepted")
		}
	}
	for _, aud := range []string{"", " ", " aud", "aud ", "aud\n", strings.Repeat("x", 513)} {
		p := valid()
		p.Audience = aud
		if err := p.Validate(); err == nil {
			t.Fatal("invalid audience accepted")
		}
	}
	p := valid()
	p.Audience = "exact-nonhex-application-audience"
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"", "owner", "*", "*@example.com", "person+*@example.com", "Owner <owner@example.com>", " owner@example.com", "owner@example.com\n"} {
		p := valid()
		p.Principals[0].Email = email
		if err := p.Validate(); err == nil {
			t.Fatal("nonspecific email accepted")
		}
	}
	for _, devices := range [][]string{nil, {"*"}, {"../worker"}, {""}} {
		p := valid()
		p.Principals[0].Devices = devices
		if err := p.Validate(); err == nil {
			t.Fatal("invalid node grants accepted")
		}
	}
	p = valid()
	p.Principals = append(p.Principals, CloudflareAccessPrincipal{Email: "owner@example.com", Devices: []string{"root"}})
	if err := p.Validate(); err == nil {
		t.Fatal("case-alias duplicate email accepted")
	}
	p = valid()
	p.Principals = nil
	if err := p.Validate(); err != nil {
		t.Fatal("deny-all policy must permit revocation", err)
	}
}
