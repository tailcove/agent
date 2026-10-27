package probe

import (
	"net/netip"
	"testing"
)

func TestPublicAddress(t *testing.T) {
	cases := map[string]bool{
		"93.184.216.34":          true,
		"8.8.8.8":                true,
		"2606:4700:4700::1111":   true,
		"127.0.0.1":              false,
		"10.0.0.5":               false,
		"172.16.0.1":             false,
		"192.168.1.1":            false,
		"169.254.169.254":        false,
		"100.64.0.1":             false,
		"0.0.0.0":                false,
		"224.0.0.1":              false,
		"255.255.255.255":        false,
		"198.18.0.1":             false,
		"192.0.2.1":              false,
		"::1":                    false,
		"fe80::1":                false,
		"fd00::1":                false,
		"::ffff:127.0.0.1":       false,
		"::ffff:169.254.169.254": false,
		"::ffff:8.8.8.8":         true,
		"64:ff9b::7f00:1":        false,
		"2001:db8::1":            false,
	}

	for text, want := range cases {
		if got := PublicAddress(netip.MustParseAddr(text)); got != want {
			t.Errorf("PublicAddress(%s) = %v, want %v", text, got, want)
		}
	}
}

func TestCheckURL(t *testing.T) {
	g := Guard{}

	allowed := []string{
		"https://example.com/health",
		"http://example.com:8080/x?y=1",
		"https://sub.example.co.uk",
		"https://93.184.216.34/",
		"https://[2606:4700:4700::1111]/",
	}

	for _, raw := range allowed {
		if _, err := g.CheckURL(raw); err != nil {
			t.Errorf("CheckURL(%q) refused: %v", raw, err)
		}
	}

	refused := []string{
		"http://127.0.0.1/", "http://127.0.0.1:6379/", "http://169.254.169.254/latest/meta-data/", "http://10.0.0.5/",
		"http://localhost/admin", "http://localhost./", "http://db/", "http://metadata.google.internal/", "http://svc.cluster.local/",
		"http://[::1]/", "http://[::ffff:169.254.169.254]/", "http://2130706433/", "http://0x7f000001/", "http://0177.0.0.1/",
		"http://127.1/", "http://example.com@127.0.0.1/", "ftp://example.com/", "gopher://127.0.0.1:6379/_PING", "file:///etc/passwd",
		"http:///nohost", "", "https://exa mple.com/", "https://example.com\\@127.0.0.1/", "http://1.2.3.4.5/", "http://example.0x1/",
	}

	for _, raw := range refused {
		if _, err := g.CheckURL(raw); err == nil {
			t.Errorf("CheckURL(%q) was allowed", raw)
		}
	}
}

func TestControlRefusesPrivateAddresses(t *testing.T) {
	g := Guard{}

	for _, address := range []string{"127.0.0.1:80", "10.1.2.3:443", "[::1]:80", "169.254.169.254:80", "not-an-address"} {
		if err := g.Control("tcp", address, nil); err == nil {
			t.Errorf("Control(%q) allowed", address)
		}
	}

	if err := g.Control("tcp", "93.184.216.34:443", nil); err != nil {
		t.Errorf("Control refused a public address: %v", err)
	}

	if err := (Guard{AllowPrivate: true}).Control("tcp", "127.0.0.1:80", nil); err != nil {
		t.Errorf("AllowPrivate did not allow: %v", err)
	}
}
