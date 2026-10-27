// Package probe is the agent's probe mode: it pulls the checks due for its
// region from Tailcove, runs them from this host, and posts the results.
package probe

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
)

// ErrNotAllowed is returned for a target the probe refuses to contact. The
// message is deliberately generic: a result is shown to the monitor's owners,
// and must not say what an internal host is or answered.
var ErrNotAllowed = errors.New("URL not allowed")

// internalSuffixes are names that only ever exist inside a private network.
var internalSuffixes = []string{
	"localhost", "local", "internal", "localdomain", "lan", "home", "home.arpa", "corp", "intranet", "private",
}

// blocked are the ranges a probe must never connect to, besides what netip
// already calls private, loopback, link-local, multicast or unspecified.
var blocked = mustPrefixes(
	"0.0.0.0/8",       // "this" network
	"100.64.0.0/10",   // carrier-grade NAT
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"240.0.0.0/4",     // reserved, and the broadcast address
	"64:ff9b::/96",    // NAT64
	"100::/64",        // discard-only
	"2001:db8::/32",   // documentation
)

func mustPrefixes(values ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(values))

	for _, value := range values {
		out = append(out, netip.MustParsePrefix(value))
	}

	return out
}

// PublicAddress reports whether ip is an address a probe may connect to: a
// global unicast address outside every blocked range. An IPv4-mapped IPv6
// address is judged as the IPv4 address it carries.
func PublicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()

	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}

	for _, prefix := range blocked {
		if prefix.Contains(ip) {
			return false
		}
	}

	return true
}

// Guard decides which targets a probe may contact. It works at two levels:
// CheckURL judges the text of a URL (scheme, userinfo, host spelling, internal
// names), and Control is installed on every dialer so the address actually
// being connected to is judged too, whatever DNS answered or however a redirect
// got there: there is no window between "vetted" and "connected".
type Guard struct {
	// AllowPrivate switches the address checks off. Only tests set it.
	AllowPrivate bool
}

// CheckURL refuses everything but a plain http(s) URL naming a host the probe
// may look up.
func (g Guard) CheckURL(raw string) (*url.URL, error) {
	if len(raw) > 8192 {
		return nil, ErrNotAllowed
	}

	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Hostname() == "" || strings.ContainsAny(raw, "\\ \t\r\n") {
		return nil, ErrNotAllowed
	}

	if err := g.CheckHost(u.Hostname()); err != nil {
		return nil, err
	}

	return u, nil
}

// CheckHost refuses an address that is not public, a name that is only
// internal, and any spelling of an address other than the canonical one (the
// operating system's resolver would read "2130706433" or "0x7f.1" as an address).
func (g Guard) CheckHost(host string) error {
	if g.AllowPrivate {
		return nil
	}

	host = strings.TrimSuffix(strings.ToLower(host), ".")

	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if PublicAddress(ip) {
			return nil
		}

		return ErrNotAllowed
	}

	labels := strings.Split(host, ".")
	last := labels[len(labels)-1]

	// A name whose last label is numeric or hex is an address in some spelling, not a name.
	if last == "" || isNumeric(last) || strings.HasPrefix(last, "0x") {
		return ErrNotAllowed
	}

	if len(labels) < 2 {
		return ErrNotAllowed
	}

	for _, suffix := range internalSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return ErrNotAllowed
		}
	}

	return nil
}

// Control is a net.Dialer.Control function: it runs with the address about to
// be connected to and refuses one that is not public.
func (g Guard) Control(_, address string, _ syscall.RawConn) error {
	if g.AllowPrivate {
		return nil
	}

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ErrNotAllowed
	}

	ip, err := netip.ParseAddr(host)
	if err != nil || !PublicAddress(ip) {
		return ErrNotAllowed
	}

	return nil
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}

	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}
