package proxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

var errDestinationDenied = errors.New("destination blocked by policy")

func normalizeHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return "", fmt.Errorf("scoped addresses are not supported")
		}
		return ip.Unmap().String(), nil
	}
	if host == "" || len(host) > 253 {
		return "", fmt.Errorf("invalid hostname")
	}
	for _, part := range strings.Split(host, ".") {
		if part == "" || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
			return "", fmt.Errorf("invalid hostname label")
		}
		for _, ch := range part {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return "", fmt.Errorf("hostname must use ASCII/punycode")
			}
		}
	}
	return host, nil
}

func authority(s, defaultPort string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(s)
	if err != nil && defaultPort != "" && !strings.Contains(s, ":") {
		host, port, err = s, defaultPort, nil
	}
	if err != nil && defaultPort != "" && strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		host, port, err = s[1:len(s)-1], defaultPort, nil
	}
	if err != nil {
		return "", "", err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("invalid port")
	}
	host, err = normalizeHost(host)
	return host, strconv.Itoa(n), err
}

func (p *Proxy) checkHost(host string) error {
	if p.config.Policy == nil {
		return nil
	}
	d := p.config.Policy.Destinations
	for _, rule := range d.DenyHosts {
		if host == rule || strings.HasPrefix(rule, "*.") && strings.HasSuffix(host, rule[1:]) {
			log.Printf("destination denied: host rule, host=%s", host)
			return errDestinationDenied
		}
	}
	for _, re := range d.regexes {
		if re.MatchString(host) {
			log.Printf("destination denied: hostname regex, host=%s", host)
			return errDestinationDenied
		}
	}
	return nil
}

func (p *Proxy) checkIP(ip netip.Addr, port string) error {
	ip = ip.Unmap()
	// Do not let the sandbox chain through this or the rollback proxy. Account
	// for local interface addresses as well as loopback and wildcard listeners.
	_, listenPort, _ := net.SplitHostPort(p.config.Listen)
	if port == "8443" || port == "8444" || listenPort != "" && port == listenPort {
		local := ip.IsLoopback() || ip.IsUnspecified()
		for _, addr := range p.localIPs {
			local = local || addr == ip
		}
		if local {
			return errDestinationDenied
		}
	}
	if p.config.Policy != nil {
		for _, prefix := range p.config.Policy.Destinations.prefixes {
			if prefix.Contains(ip) {
				return errDestinationDenied
			}
		}
	}
	return nil
}

// Resolve once, validate every candidate before making any connection, and
// pass only numeric addresses to the dialer. TLS still verifies the hostname.
func (p *Proxy) checkedDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := authority(address, "")
	if err != nil {
		return nil, err
	}
	if err := p.checkHost(host); err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		lookup := p.lookupIP
		if lookup == nil {
			lookup = net.DefaultResolver.LookupNetIP
		}
		ips, err = lookup(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("destination has no addresses")
	}
	for _, ip := range ips {
		if !ip.IsValid() || ip.Zone() != "" {
			return nil, errDestinationDenied
		}
		if err := p.checkIP(ip, port); err != nil {
			log.Printf("destination denied: IP or proxy-loop rule, host=%s", host)
			return nil, err
		}
	}
	dial := p.dialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, ip := range ips {
		var c net.Conn
		c, err = dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return c, nil
		}
	}
	return nil, err
}
