package security

// ssrf.go — SSRF / DNS-rebinding guard for HTTP-issuing tools (WebFetch,
// WebBrowse, agentic_fetch, hooks). Blocks private, link-local, and
// cloud-metadata IP ranges so the model can't be tricked into reaching
// 169.254.169.254 (EC2 / GCE metadata) or internal infra via a
// model-controlled URL.
//
// Loopback (127.0.0.0/8 and ::1) is INTENTIONALLY ALLOWED — local dev
// servers, hook receivers, and "fetch the docs site I'm running" are
// the primary use cases, and require it.
//
// Mirrors claude-code-sourcemap `restored-src/src/utils/hooks/ssrfGuard.ts`
// IP-range list (the IETF-canonical RFC1918 + RFC6598 + IPv4-mapped IPv6
// + cloud metadata set).
//
// Anti-rebinding: GuardedDialContext does the DNS lookup ITSELF and
// validates the resolved IPs before opening the socket. The validated
// IP is the one Dial connects to — there's no rebinding window between
// validation and connect.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// BlockedError describes the reason a connection was refused. Returned
// by GuardedDialContext so callers (and tests) can distinguish "SSRF
// blocked you" from generic dial errors. errors.Is(err, ErrBlocked)
// returns true for any blocked-address path.
type BlockedError struct {
	Hostname string // original hostname the caller asked for
	Address  string // resolved IP that triggered the block
	Reason   string // short tag (e.g. "rfc1918", "link-local", "metadata")
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("ssrf: refusing to connect to %s (resolved to %s — %s; loopback 127.0.0.1/::1 is allowed for local dev)",
		e.Hostname, e.Address, e.Reason)
}

// ErrBlocked is the sentinel returned via errors.Is for any blocked
// connection. Useful in webfetch.go when we want to surface a clean
// "blocked by SSRF guard" message instead of leaking the resolved IP.
var ErrBlocked = errors.New("ssrf: address blocked")

func (e *BlockedError) Is(target error) bool { return target == ErrBlocked }

// IsBlockedIP reports whether ip falls in a range HTTP-issuing tools
// must not reach. Pure function, no I/O — safe to call on every
// resolved address. Treats nil / unspecified IPs as blocked.
//
// Block list (mirroring claude-code-sourcemap):
//
//	IPv4:
//	  0.0.0.0/8        unspecified ("this network")
//	  10.0.0.0/8       RFC1918 private
//	  100.64.0.0/10    RFC6598 CGNAT (Alibaba metadata 100.100.100.200)
//	  169.254.0.0/16   link-local (cloud metadata 169.254.169.254)
//	  172.16.0.0/12    RFC1918 private
//	  192.168.0.0/16   RFC1918 private
//	IPv6:
//	  ::               unspecified
//	  fc00::/7         unique local (fc00–fdff)
//	  fe80::/10        link-local
//	  ::ffff:<v4>      IPv4-mapped IPv6 — unwrap and re-check as v4
//
// Allowed (returns false):
//
//	127.0.0.0/8      loopback (local dev)
//	::1              loopback
//	all public IPs
func IsBlockedIP(ip net.IP) (bool, string) {
	if ip == nil {
		return true, "nil-ip"
	}
	// IPv4-mapped IPv6 (::ffff:a.b.c.d) — Go normalises to a 16-byte
	// slice with the v4 in the last 4 bytes. To4() returns the v4 form
	// without losing the mapped bit, so v4 rules apply uniformly.
	if v4 := ip.To4(); v4 != nil {
		return isBlockedV4(v4)
	}
	v6 := ip.To16()
	if v6 == nil {
		return true, "invalid-ip"
	}
	return isBlockedV6(v6)
}

func isBlockedV4(ip net.IP) (bool, string) {
	a, b := ip[0], ip[1]
	switch {
	case a == 127:
		return false, "" // loopback explicitly allowed
	case a == 0:
		return true, "unspecified-v4"
	case a == 10:
		return true, "rfc1918"
	case a == 169 && b == 254:
		return true, "link-local-metadata"
	case a == 172 && b >= 16 && b <= 31:
		return true, "rfc1918"
	case a == 100 && b >= 64 && b <= 127:
		return true, "rfc6598-cgnat"
	case a == 192 && b == 168:
		return true, "rfc1918"
	}
	return false, ""
}

func isBlockedV6(ip net.IP) (bool, string) {
	// ::1 loopback explicitly allowed.
	if ip.IsLoopback() {
		return false, ""
	}
	// :: unspecified.
	if ip.IsUnspecified() {
		return true, "unspecified-v6"
	}
	// fc00::/7 unique local addresses (fc00 through fdff).
	if ip[0] == 0xfc || ip[0] == 0xfd {
		return true, "unique-local"
	}
	// fe80::/10 link-local — first 10 bits 1111111010, i.e. fe80–febf.
	if ip[0] == 0xfe && (ip[1]&0xc0) == 0x80 {
		return true, "link-local-v6"
	}
	return false, ""
}

// GuardedDialContext is a drop-in DialContext for *http.Transport that
// resolves the hostname through Go's standard resolver, validates EVERY
// returned IP against IsBlockedIP, and dials only the allowed ones.
// If any resolved address is blocked, it returns *BlockedError rather than
// allowing a mixed public/private answer to become a rebinding path.
//
// IP literals in the address are validated directly without DNS — this
// closes the "model passes 169.254.169.254 directly" hole.
//
// Anti-rebinding: the validated IP is the one Dial connects to. There's
// no window between "resolver said it's public" and "socket connects to
// some private IP" because we hand the literal IP to net.Dial.
func GuardedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return guardedDialContext(ctx, network, addr, net.DefaultResolver, netDial)
}

type ipAddrResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

// guardedDialContext contains the resolver-to-dial binding used by
// GuardedDialContext. Keeping the resolver and dialer explicit here lets tests
// prove that a mixed public/private DNS answer is rejected before any socket
// is opened; production always supplies net.DefaultResolver and netDial.
func guardedDialContext(
	ctx context.Context,
	network, addr string,
	resolver ipAddrResolver,
	dial dialContextFunc,
) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	// IP literal short-circuit: validate without resolving.
	if ip := net.ParseIP(host); ip != nil {
		if blocked, why := IsBlockedIP(ip); blocked {
			return nil, &BlockedError{Hostname: host, Address: ip.String(), Reason: why}
		}
		return dial(ctx, network, net.JoinHostPort(ip.String(), port))
	}
	// Hostname: resolve and validate every result. Use the default
	// resolver (respects /etc/hosts, NSS, mDNS), then dial the first
	// allowed IP. Two-second resolver timeout matches Go's stdlib
	// http.Transport default behaviour.
	rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
	defer rcancel()
	addrs, err := resolver.LookupIPAddr(rctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
	}
	// Reject if ANY resolved IP is blocked — a multi-A-record hostname
	// where one record is private is suspicious and not worth the
	// "try the next one" complexity. claude-code-sourcemap takes the
	// same hard-fail stance.
	for _, a := range addrs {
		if blocked, why := IsBlockedIP(a.IP); blocked {
			return nil, &BlockedError{Hostname: host, Address: a.IP.String(), Reason: why}
		}
	}
	// All clear — dial the first IP literal so the connection target
	// matches the validated IP exactly (no second resolution).
	first := addrs[0].IP.String()
	if strings.Contains(first, ":") {
		first = "[" + first + "]"
	}
	return dial(ctx, network, first+":"+port)
}

// netDial is split out so tests can stub it. Default just calls
// net.Dialer.DialContext with a 5-second connect timeout.
var netDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 5 * time.Second}
	return d.DialContext(ctx, network, addr)
}
