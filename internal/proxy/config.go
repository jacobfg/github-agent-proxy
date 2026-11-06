package proxy

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Listen, Token, BranchPrefix, CertFile, KeyFile string
	Organizations                                  map[string]bool
	Users                                          map[string]bool
	MaxPushBytes                                   int64
	Policy                                         *Policy
	CACertFile, CAKeyFile                          string
}

// RuntimeOptions contains operational inputs, before loading policy and secrets.
type RuntimeOptions struct {
	PolicyFile, Listen, Token, TokenFile     string
	CertFile, KeyFile, CACertFile, CAKeyFile string
	MaxPushBytes                             string
	AllowContainerBind                       bool
}

func RuntimeOptionsFromEnv() RuntimeOptions {
	return RuntimeOptions{
		PolicyFile: os.Getenv("POLICY_FILE"), Listen: os.Getenv("LISTEN_ADDR"),
		Token: os.Getenv("GITHUB_TOKEN"), TokenFile: os.Getenv("GITHUB_TOKEN_FILE"),
		CertFile: os.Getenv("TLS_CERT_FILE"), KeyFile: os.Getenv("TLS_KEY_FILE"),
		CACertFile: os.Getenv("INTERCEPT_CA_CERT_FILE"), CAKeyFile: os.Getenv("INTERCEPT_CA_KEY_FILE"),
		MaxPushBytes: os.Getenv("MAX_PUSH_BYTES"), AllowContainerBind: os.Getenv("ALLOW_CONTAINER_BIND") == "1",
	}
}

func FromEnv() (Config, error) {
	return FromOptions(RuntimeOptionsFromEnv())
}

func FromOptions(options RuntimeOptions) (Config, error) {
	c := Config{Listen: options.Listen, Token: options.Token, CertFile: options.CertFile, KeyFile: options.KeyFile, MaxPushBytes: 1 << 30}
	c.CACertFile, c.CAKeyFile = options.CACertFile, options.CAKeyFile
	file := options.PolicyFile
	if strings.TrimSpace(file) == "" {
		return c, fmt.Errorf("POLICY_FILE is required (or use --policy-file); create a policy with github-agent-proxy init-policy --output PATH")
	}
	var err error
	c.Policy, err = loadPolicy(file)
	if err != nil {
		return c, err
	}
	c.Organizations = loginSet(c.Policy.GitHub.AllowedOrgs)
	c.Users = loginSet(c.Policy.GitHub.AllowedUsers)
	c.BranchPrefix = c.Policy.GitHub.BranchPrefix
	if c.interceptAll() && (c.CACertFile == "" || c.CAKeyFile == "") {
		return c, fmt.Errorf("interception requires INTERCEPT_CA_CERT_FILE and INTERCEPT_CA_KEY_FILE")
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8443"
	}
	if file := options.TokenFile; file != "" {
		if c.Token != "" {
			return c, fmt.Errorf("set only one of GITHUB_TOKEN and GITHUB_TOKEN_FILE")
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return c, fmt.Errorf("read token file: %w", err)
		}
		c.Token = strings.TrimSpace(string(b))
	}
	if value := options.MaxPushBytes; value != "" {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 1024 {
			return c, fmt.Errorf("MAX_PUSH_BYTES must be at least 1024")
		}
		c.MaxPushBytes = n
	}
	if c.Token == "" || strings.ContainsAny(c.Token, "\r\n") {
		return c, fmt.Errorf("a GitHub token is required")
	}
	if c.CertFile == "" || c.KeyFile == "" {
		return c, fmt.Errorf("TLS_CERT_FILE and TLS_KEY_FILE are required")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return c, fmt.Errorf("invalid LISTEN_ADDR")
	}
	ip := net.ParseIP(host)
	if ip == nil || (!ip.IsLoopback() && !options.AllowContainerBind) {
		return c, fmt.Errorf("LISTEN_ADDR must be a loopback IP; isolated containers require ALLOW_CONTAINER_BIND=1")
	}
	return c, nil
}

func loginSet(values []string) map[string]bool {
	logins := map[string]bool{}
	for _, login := range values {
		if login = strings.ToLower(strings.TrimSpace(login)); login != "" {
			logins[login] = true
		}
	}
	return logins
}

// GitHub's owner type determines which allowlist applies. A username in
// the organisation list must never grant access to personal repositories.
func (p *Proxy) ownerAllowed(ownerType, login string) bool {
	switch ownerType {
	case "Organization":
		return p.config.Organizations[strings.ToLower(login)]
	case "User":
		return p.config.Users[strings.ToLower(login)]
	default:
		return false
	}
}

func validBranch(s string) bool {
	if s == "" || s == "@" || strings.HasPrefix(s, "-") || strings.HasSuffix(s, ".") || strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.ContainsAny(s, " ~^:?*[\\\x00\r\n\t") {
		return false
	}
	for _, ch := range s {
		if ch < 32 || ch == 127 {
			return false
		}
	}
	for _, p := range strings.Split(s, "/") {
		if p == "" || strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".lock") {
			return false
		}
	}
	return true
}

func (p *Proxy) branch(s string) bool {
	return validBranch(s) && strings.HasPrefix(s, p.config.BranchPrefix)
}
