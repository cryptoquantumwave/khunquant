package utils

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ErrRestrictedTarget is wrapped into the error returned by a SafeDialContext
// dialer when a destination is rejected by SSRF policy. Callers can use
// errors.Is(err, ErrRestrictedTarget) to distinguish "the caller pointed us at
// a disallowed address" (a client error) from an ordinary network failure
// against an allowed address (a server/upstream error).
var ErrRestrictedTarget = errors.New("target address is private, restricted, or not permitted for this request")

// IsPrivateOrRestrictedIP reports whether ip should never be reached by an
// outbound request whose destination is fully or partially controlled by a
// remote caller: RFC 1918, loopback, link-local (including the cloud
// metadata address 169.254.169.254), carrier-grade NAT, IPv6 unique-local
// (fc00::/7), 6to4 (2002::/16), and Teredo (2001:0000::/32).
//
// This is shared by every SSRF-guarded outbound client in the codebase (the
// web-fetch/search tools in pkg/tools, and the model-catalog/test-inline
// endpoints in web/backend/api) so the address classification lives in one
// place.
func IsPrivateOrRestrictedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}

	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}

	if ip4 := ip.To4(); ip4 != nil {
		// IPv4 private, loopback, link-local, and carrier-grade NAT ranges.
		if ip4[0] == 10 ||
			ip4[0] == 127 ||
			ip4[0] == 0 ||
			(ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31) ||
			(ip4[0] == 192 && ip4[1] == 168) ||
			(ip4[0] == 169 && ip4[1] == 254) ||
			(ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127) {
			return true
		}
		return false
	}

	if len(ip) == net.IPv6len {
		// IPv6 unique local addresses (fc00::/7)
		if (ip[0] & 0xfe) == 0xfc {
			return true
		}
		// 6to4 addresses (2002::/16): check the embedded IPv4 at bytes [2:6].
		if ip[0] == 0x20 && ip[1] == 0x02 {
			embedded := net.IPv4(ip[2], ip[3], ip[4], ip[5])
			return IsPrivateOrRestrictedIP(embedded)
		}
		// Teredo (2001:0000::/32): client IPv4 is at bytes [12:16], XOR-inverted.
		if ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x00 && ip[3] == 0x00 {
			client := net.IPv4(ip[12]^0xff, ip[13]^0xff, ip[14]^0xff, ip[15]^0xff)
			return IsPrivateOrRestrictedIP(client)
		}
	}

	return false
}

// IsLinkLocalOrMetadataIP reports whether ip is a link-local address:
// 169.254.0.0/16 (which includes the cloud metadata address
// 169.254.169.254) or fe80::/10. These must never be dialed, even through an
// explicit allowlist or an operator opt-in — there is never a legitimate
// reason for a server-initiated fetch to reach a link-local address or the
// cloud metadata service.
func IsLinkLocalOrMetadataIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

// SafeDialPolicy controls which otherwise-restricted destinations
// NewSafeDialContext permits.
//
// Policy: a private/loopback destination is only ever allowed when AllowHost
// says so for that specific host. Link-local addresses and the cloud
// metadata IP are rejected unconditionally, regardless of AllowHost — see
// IsLinkLocalOrMetadataIP.
type SafeDialPolicy struct {
	// AllowHost, when non-nil, is consulted with the dial address's hostname
	// exactly as it appeared before DNS resolution (a literal IP or a
	// hostname, lowercased). Returning true permits that host to reach a
	// private/loopback IP that IsPrivateOrRestrictedIP would otherwise block.
	AllowHost func(host string) bool
}

// allows reports whether ip may be dialed for the given pre-resolution host,
// under this policy.
func (p SafeDialPolicy) allows(host string, ip net.IP) bool {
	if ip == nil {
		return false
	}
	if IsLinkLocalOrMetadataIP(ip) {
		return false
	}
	if !IsPrivateOrRestrictedIP(ip) {
		return true
	}
	return p.AllowHost != nil && p.AllowHost(host)
}

// NewSafeDialContext returns a net.Dialer-backed DialContext suitable for
// http.Transport.DialContext (or direct use, e.g. for a raw TCP probe). It
// re-resolves DNS at connect time — closing the TOCTOU window a pre-flight
// URL check would otherwise leave open to DNS rebinding, where a hostname
// resolves to a public address during validation but a private one at
// connect time — and applies policy to the resolved address before dialing.
func NewSafeDialContext(dialer *net.Dialer, policy SafeDialPolicy) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid target address %q: %w", address, err)
		}
		if host == "" {
			return nil, fmt.Errorf("empty target host")
		}

		if ip := net.ParseIP(host); ip != nil {
			if !policy.allows(host, ip) {
				return nil, fmt.Errorf("%w: %s", ErrRestrictedTarget, host)
			}
			return dialer.DialContext(ctx, network, address)
		}

		ipAddrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve %s: %w", host, err)
		}

		attempted := 0
		var lastErr error
		for _, ipAddr := range ipAddrs {
			if !policy.allows(host, ipAddr.IP) {
				continue
			}
			attempted++
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}

		if attempted == 0 {
			return nil, fmt.Errorf("%w: all resolved addresses for %s are restricted", ErrRestrictedTarget, host)
		}
		if lastErr != nil {
			return nil, fmt.Errorf("failed connecting to allowed addresses for %s: %w", host, lastErr)
		}
		return nil, fmt.Errorf("failed connecting to allowed addresses for %s", host)
	}
}

// NewSafeHTTPClient returns an http.Client whose transport dials through
// NewSafeDialContext with the strictest policy: every private, loopback,
// link-local, and cloud-metadata destination is blocked. This is the default
// for outbound fetches whose target should never legitimately be a private
// network address.
func NewSafeHTTPClient(timeout time.Duration) *http.Client {
	return NewPolicyHTTPClient(timeout, SafeDialPolicy{})
}

// NewPolicyHTTPClient is like NewSafeHTTPClient but takes an explicit
// SafeDialPolicy, for callers that must make a controlled, documented
// exception — e.g. a caller-supplied endpoint that may only be a LAN/loopback
// host when it matches an operator's already-saved configuration, or an
// explicit and CSRF-gated opt-in for one specific host.
func NewPolicyHTTPClient(timeout time.Duration, policy SafeDialPolicy) *http.Client {
	dialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		DialContext:         NewSafeDialContext(dialer, policy),
		MaxIdleConns:        10,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		const maxRedirects = 5
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return nil
	}
	return client
}
