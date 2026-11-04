package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"regexp"
	"strings"
)

// Policy is immutable after startup. A nil repository allowlist means no
// additional restriction; a non-nil empty list denies every repository write.
type Policy struct {
	HTTPSMode    string            `json:"https_mode"`
	Destinations DestinationPolicy `json:"destinations"`
	GitHub       GitHubPolicy      `json:"github"`
}

type DestinationPolicy struct {
	DenyHosts       []string `json:"deny_hosts,omitempty"`
	DenyHostRegexes []string `json:"deny_host_regexes,omitempty"`
	DenyIPs         []string `json:"deny_ips,omitempty"`
	regexes         []*regexp.Regexp
	prefixes        []netip.Prefix
}

type GitHubPolicy struct {
	AllowedOrgs     []string `json:"allowed_orgs"`
	AllowedUsers    []string `json:"allowed_users"`
	BranchPrefix    string   `json:"branch_prefix"`
	WriteAllowRepos []string `json:"write_allow_repos"`
	WriteDenyRepos  []string `json:"write_deny_repos"`
}

// Reject duplicate keys and null values as well as unknown fields. Otherwise a
// typo or an ambiguous merge could silently broaden an authorization policy.
func uniquePolicyJSON(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("null is not permitted in policy")
	}
	switch t {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid policy key")
			}
			seen[name] = true
			if err := uniquePolicyJSON(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
	case json.Delim('['):
		for d.More() {
			if err := uniquePolicyJSON(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
	}
	return err
}

func loadPolicy(file string) (*Policy, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read policy: %w", err)
	}
	return parsePolicy(b)
}

func parsePolicy(b []byte) (*Policy, error) {
	if len(b) > 1<<20 {
		return nil, fmt.Errorf("policy exceeds 1 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	if err := uniquePolicyJSON(d); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing policy data")
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var p Policy
	if err := d.Decode(&p); err != nil {
		return nil, err
	}
	if p.HTTPSMode != "intercept" && p.HTTPSMode != "tunnel" {
		return nil, fmt.Errorf("https_mode must be intercept or tunnel")
	}
	login := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*$`)
	for _, list := range [][]string{p.GitHub.AllowedOrgs, p.GitHub.AllowedUsers} {
		for i, s := range list {
			if !login.MatchString(s) {
				return nil, fmt.Errorf("invalid GitHub owner")
			}
			list[i] = strings.ToLower(s)
		}
	}
	if !strings.HasSuffix(p.GitHub.BranchPrefix, "/") || !validBranch(p.GitHub.BranchPrefix+"probe") {
		return nil, fmt.Errorf("invalid branch_prefix namespace")
	}
	repo := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*/[a-zA-Z0-9_.-]+$`)
	for _, list := range [][]string{p.GitHub.WriteAllowRepos, p.GitHub.WriteDenyRepos} {
		for i, s := range list {
			if !repo.MatchString(s) || strings.HasSuffix(s, "/.") || strings.HasSuffix(s, "/..") {
				return nil, fmt.Errorf("repository rules must be exact owner/repo names")
			}
			list[i] = strings.ToLower(s)
		}
	}
	for i, s := range p.Destinations.DenyHosts {
		wild := strings.HasPrefix(s, "*.")
		h, err := normalizeHost(strings.TrimPrefix(s, "*."))
		if err != nil {
			return nil, fmt.Errorf("invalid deny_hosts entry: %w", err)
		}
		if wild {
			if _, err := netip.ParseAddr(h); err == nil {
				return nil, fmt.Errorf("IP wildcards are not supported")
			}
			h = "*." + h
		}
		p.Destinations.DenyHosts[i] = h
	}
	for _, s := range p.Destinations.DenyHostRegexes {
		r, err := regexp.Compile("^(?:" + s + ")$")
		if err != nil {
			return nil, fmt.Errorf("invalid hostname regex: %w", err)
		}
		p.Destinations.regexes = append(p.Destinations.regexes, r)
	}
	for i, s := range p.Destinations.DenyIPs {
		prefix, err := netip.ParsePrefix(s)
		if err != nil {
			ip, e := netip.ParseAddr(s)
			if e != nil || ip.Zone() != "" {
				return nil, fmt.Errorf("invalid deny_ips address or CIDR")
			}
			ip = ip.Unmap()
			prefix = netip.PrefixFrom(ip, ip.BitLen())
		} else if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return nil, fmt.Errorf("mapped IPv4 CIDR must have at least 96 prefix bits")
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		prefix = prefix.Masked()
		p.Destinations.prefixes = append(p.Destinations.prefixes, prefix)
		p.Destinations.DenyIPs[i] = prefix.String()
	}
	return &p, nil
}

func (p *Proxy) repositoryAllowed(ownerType, owner, name string) bool {
	if !p.ownerAllowed(ownerType, owner) {
		return false
	}
	if p.config.Policy == nil {
		return true
	}
	rule := p.config.Policy.GitHub
	full := strings.ToLower(owner + "/" + name)
	for _, s := range rule.WriteDenyRepos {
		if s == full {
			return false
		}
	}
	if rule.WriteAllowRepos == nil {
		return true
	}
	for _, s := range rule.WriteAllowRepos {
		if s == full {
			return true
		}
	}
	return false
}

func (c Config) interceptAll() bool { return c.Policy != nil && c.Policy.HTTPSMode == "intercept" }
