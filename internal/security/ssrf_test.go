package security

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

type stubIPResolver struct {
	addrs []net.IPAddr
	err   error
}

func (s stubIPResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return s.addrs, s.err
}

// ─── IsBlockedIP — IPv4 ──────────────────────────────────────────

func TestIsBlockedIP_LoopbackAllowed(t *testing.T) {
	cases := []string{"127.0.0.1", "127.255.255.254", "127.10.20.30"}
	for _, c := range cases {
		if blocked, _ := IsBlockedIP(net.ParseIP(c)); blocked {
			t.Errorf("loopback %s must be allowed", c)
		}
	}
}

func TestIsBlockedIP_RFC1918Blocked(t *testing.T) {
	cases := []struct{ ip, why string }{
		{"10.0.0.1", "rfc1918"},
		{"10.255.255.255", "rfc1918"},
		{"172.16.0.1", "rfc1918"},
		{"172.31.255.255", "rfc1918"},
		{"192.168.1.1", "rfc1918"},
		{"192.168.255.255", "rfc1918"},
	}
	for _, c := range cases {
		blocked, why := IsBlockedIP(net.ParseIP(c.ip))
		if !blocked {
			t.Errorf("rfc1918 %s must be blocked", c.ip)
		}
		if why != c.why {
			t.Errorf("%s reason: got %q, want %q", c.ip, why, c.why)
		}
	}
}

func TestIsBlockedIP_RFC1918BoundaryCases(t *testing.T) {
	// 172.15 and 172.32 are NOT private (the range is 172.16–172.31).
	if blocked, _ := IsBlockedIP(net.ParseIP("172.15.0.1")); blocked {
		t.Error("172.15.0.1 is public, must not be blocked")
	}
	if blocked, _ := IsBlockedIP(net.ParseIP("172.32.0.1")); blocked {
		t.Error("172.32.0.1 is public, must not be blocked")
	}
}

func TestIsBlockedIP_LinkLocalMetadata(t *testing.T) {
	// 169.254.169.254 is the EC2/GCE metadata endpoint — the canonical
	// SSRF target. Must be blocked.
	blocked, why := IsBlockedIP(net.ParseIP("169.254.169.254"))
	if !blocked {
		t.Fatal("EC2/GCE metadata IP 169.254.169.254 must be blocked")
	}
	if why != "link-local-metadata" {
		t.Errorf("metadata reason: got %q, want link-local-metadata", why)
	}
}

func TestIsBlockedIP_CGNATAlibabaMetadata(t *testing.T) {
	// 100.100.100.200 is Alibaba Cloud's metadata endpoint, inside the
	// RFC6598 CGNAT range. Must be blocked.
	if blocked, _ := IsBlockedIP(net.ParseIP("100.100.100.200")); !blocked {
		t.Error("Alibaba metadata 100.100.100.200 must be blocked")
	}
	// 100.63 just below CGNAT, 100.128 just above — both public.
	if blocked, _ := IsBlockedIP(net.ParseIP("100.63.255.255")); blocked {
		t.Error("100.63.255.255 is public (just below CGNAT)")
	}
	if blocked, _ := IsBlockedIP(net.ParseIP("100.128.0.0")); blocked {
		t.Error("100.128.0.0 is public (just above CGNAT)")
	}
}

func TestIsBlockedIP_PublicV4Allowed(t *testing.T) {
	cases := []string{"8.8.8.8", "1.1.1.1", "142.250.80.46", "104.16.0.1"}
	for _, c := range cases {
		if blocked, _ := IsBlockedIP(net.ParseIP(c)); blocked {
			t.Errorf("public %s must be allowed", c)
		}
	}
}

func TestIsBlockedIP_UnspecifiedV4Blocked(t *testing.T) {
	// 0.0.0.0 itself + the whole 0.0.0.0/8 range (rare in practice but
	// some kernels route it specially).
	if blocked, _ := IsBlockedIP(net.ParseIP("0.0.0.0")); !blocked {
		t.Error("0.0.0.0 unspecified must be blocked")
	}
	if blocked, _ := IsBlockedIP(net.ParseIP("0.1.2.3")); !blocked {
		t.Error("0.1.2.3 (in 0.0.0.0/8) must be blocked")
	}
}

// ─── IsBlockedIP — IPv6 ──────────────────────────────────────────

func TestIsBlockedIP_IPv6Loopback(t *testing.T) {
	if blocked, _ := IsBlockedIP(net.ParseIP("::1")); blocked {
		t.Error("::1 must be allowed")
	}
}

func TestIsBlockedIP_IPv6Unspecified(t *testing.T) {
	if blocked, _ := IsBlockedIP(net.ParseIP("::")); !blocked {
		t.Error(":: must be blocked")
	}
}

func TestIsBlockedIP_IPv6UniqueLocal(t *testing.T) {
	// fc00::/7 covers fc00 through fdff.
	cases := []string{"fc00::1", "fd12:3456:789a::1", "fdff::1"}
	for _, c := range cases {
		if blocked, _ := IsBlockedIP(net.ParseIP(c)); !blocked {
			t.Errorf("%s (fc00::/7 ULA) must be blocked", c)
		}
	}
}

func TestIsBlockedIP_IPv6LinkLocal(t *testing.T) {
	cases := []string{"fe80::1", "feaf::1", "febf::1"}
	for _, c := range cases {
		if blocked, _ := IsBlockedIP(net.ParseIP(c)); !blocked {
			t.Errorf("%s (fe80::/10 link-local) must be blocked", c)
		}
	}
	// fec0:: was deprecated site-local — out of fe80::/10, currently allowed.
	// We don't add a special rule for it.
}

func TestIsBlockedIP_IPv4MappedIPv6(t *testing.T) {
	// ::ffff:169.254.169.254 — the IPv4-mapped form of EC2 metadata.
	// Without v4-mapping detection this would slip through as a "public
	// IPv6 address". Test the dotted form.
	if blocked, _ := IsBlockedIP(net.ParseIP("::ffff:169.254.169.254")); !blocked {
		t.Error("::ffff:169.254.169.254 (mapped EC2 metadata) must be blocked")
	}
	// And the hex-group form (a9fe = 169.254, a9fe = 169.254). Note
	// that net.ParseIP recognises both forms and produces an identical
	// 16-byte slice, so the check fires the same way.
	if blocked, _ := IsBlockedIP(net.ParseIP("::ffff:a9fe:a9fe")); !blocked {
		t.Error("::ffff:a9fe:a9fe (hex form of mapped EC2 metadata) must be blocked")
	}
	// And mapped private RFC1918.
	if blocked, _ := IsBlockedIP(net.ParseIP("::ffff:10.0.0.1")); !blocked {
		t.Error("::ffff:10.0.0.1 (mapped RFC1918) must be blocked")
	}
}

func TestIsBlockedIP_PublicV6Allowed(t *testing.T) {
	cases := []string{"2001:4860:4860::8888", "2606:4700:4700::1111"}
	for _, c := range cases {
		if blocked, _ := IsBlockedIP(net.ParseIP(c)); blocked {
			t.Errorf("public %s must be allowed", c)
		}
	}
}

func TestIsBlockedIP_NilIPBlocked(t *testing.T) {
	if blocked, _ := IsBlockedIP(nil); !blocked {
		t.Error("nil IP must be blocked (defensive)")
	}
}

// ─── BlockedError shape ──────────────────────────────────────────

func TestBlockedError_IsErrBlocked(t *testing.T) {
	err := &BlockedError{Hostname: "foo", Address: "10.0.0.1", Reason: "rfc1918"}
	if !errors.Is(err, ErrBlocked) {
		t.Error("BlockedError must be errors.Is(err, ErrBlocked)")
	}
}

func TestBlockedError_MessageMentionsLoopbackAllowed(t *testing.T) {
	err := &BlockedError{Hostname: "h", Address: "10.0.0.1", Reason: "rfc1918"}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error message should hint loopback is allowed: %q", err.Error())
	}
}

// ─── GuardedDialContext — IP literal short-circuit ───────────────

func TestGuardedDialContext_BlocksLiteralMetadataIP(t *testing.T) {
	// Stub netDial to fail loudly if it's reached — this test asserts
	// the IP literal path NEVER reaches Dial for blocked addresses.
	orig := netDial
	defer func() { netDial = orig }()
	netDial = func(_ context.Context, _, _ string) (net.Conn, error) {
		t.Fatal("netDial must not be called for blocked literal")
		return nil, nil
	}

	_, err := GuardedDialContext(context.Background(), "tcp", "169.254.169.254:80")
	if err == nil {
		t.Fatal("expected block, got nil error")
	}
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("err should be ErrBlocked, got %T: %v", err, err)
	}
}

func TestGuardedDialContext_AllowsLiteralLoopback(t *testing.T) {
	// Stub netDial to capture what address it receives. Loopback must
	// pass through to dial.
	orig := netDial
	defer func() { netDial = orig }()
	gotAddr := ""
	netDial = func(_ context.Context, _, addr string) (net.Conn, error) {
		gotAddr = addr
		return nil, errors.New("test stub no real connection")
	}
	_, _ = GuardedDialContext(context.Background(), "tcp", "127.0.0.1:8080")
	if gotAddr != "127.0.0.1:8080" {
		t.Errorf("loopback should reach Dial unchanged: got addr %q", gotAddr)
	}
}

func TestGuardedDialContext_AllowsPublicV4Literal(t *testing.T) {
	orig := netDial
	defer func() { netDial = orig }()
	called := false
	netDial = func(_ context.Context, _, _ string) (net.Conn, error) {
		called = true
		return nil, errors.New("stub")
	}
	_, _ = GuardedDialContext(context.Background(), "tcp", "8.8.8.8:443")
	if !called {
		t.Error("public IP literal must reach Dial")
	}
}

func TestGuardedDialContext_BlocksLiteralRFC1918(t *testing.T) {
	_, err := GuardedDialContext(context.Background(), "tcp", "10.0.0.5:80")
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("RFC1918 literal must be blocked, got: %v", err)
	}
}

func TestGuardedDialContext_BlocksMappedV6MetadataLiteral(t *testing.T) {
	_, err := GuardedDialContext(context.Background(), "tcp", "[::ffff:169.254.169.254]:80")
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("mapped v6 metadata literal must be blocked, got: %v", err)
	}
}

func TestGuardedDialContext_RejectsBadAddress(t *testing.T) {
	_, err := GuardedDialContext(context.Background(), "tcp", "this-is-not-an-addr")
	if err == nil {
		t.Error("bad addr should error from net.SplitHostPort")
	}
}

func TestGuardedDialContext_BlocksMixedPublicPrivateDNSWithoutDialing(t *testing.T) {
	resolver := stubIPResolver{addrs: []net.IPAddr{
		{IP: net.ParseIP("93.184.216.34")},
		{IP: net.ParseIP("169.254.169.254")},
	}}
	called := false
	_, err := guardedDialContext(context.Background(), "tcp", "rebind.example:80", resolver,
		func(context.Context, string, string) (net.Conn, error) {
			called = true
			return nil, errors.New("must not dial")
		})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("mixed public/private DNS response must be blocked, got %v", err)
	}
	if called {
		t.Fatal("dialer was called after a private rebinding candidate was resolved")
	}
}
