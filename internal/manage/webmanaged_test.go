package manage

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/abolm55/simorgh/config"
	"github.com/abolm55/simorgh/internal/manage/host"
)

// managedWebFixture builds a NewTunnel form for a managed transport with a
// fake helper binary and a fixture certificate, so tests never touch the
// real helper paths.
func managedWebFixture(t *testing.T, role, transport string) NewTunnel {
	t.Helper()
	binary, cert, key := managedWizardFixture(t)
	n := NewTunnel{
		Role: "server", Transport: transport,
		Name:       "web-managed-" + transport + "-" + randomToken(8),
		TunnelPort: "4463", Token: strings.Repeat("t", 32),
		Ports: "8443", Preset: "",
		Managed: &ManagedCarrier{Binary: binary, Internal: "13080"},
	}
	if role == "client" {
		n.Role = "client"
		n.ServerAddr = "203.0.113.7"
		n.Ports = ""
	}
	n.Managed.Certificate, n.Managed.Key = cert, key
	return n
}

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("managed HTTPS transports require Linux")
	}
}

// The web form must accept the managed transports the CLI offers.
func TestTransportFamiliesIncludesHTTPS(t *testing.T) {
	found := map[string]bool{}
	for _, f := range TransportFamilies() {
		for _, e := range f.Entries {
			found[e.Value] = true
		}
	}
	for _, want := range []string{"naive", "xhttp", "reality"} {
		if !found[want] {
			t.Errorf("TransportFamilies is missing %q", want)
		}
	}
}

// A naive server form builds a spec whose carrier is plain TCP on loopback
// with the helper owning the public endpoint.
func TestWebManagedNaiveServerBuilds(t *testing.T) {
	requireLinux(t)
	n := managedWebFixture(t, "server", "naive")
	n.Managed.Username = "u1"
	n.Managed.Password = "p1"

	s, err := specFromNew(n)
	if err != nil {
		t.Fatalf("specFromNew: %v", err)
	}
	if s.Transport != "tcp" {
		t.Errorf("carrier = %q, want tcp (the helper wraps it)", s.Transport)
	}
	if s.BindAddr != "127.0.0.1:13080" {
		t.Errorf("BindAddr = %q, want 127.0.0.1:13080", s.BindAddr)
	}
	if !s.NaiveServer.Enabled() {
		t.Fatal("NaiveServer was not configured")
	}
	if s.NaiveServer.Listen != "[::]:4463" && s.NaiveServer.Listen != "0.0.0.0:4463" {
		t.Errorf("Listen = %q, want the public endpoint", s.NaiveServer.Listen)
	}
	if s.NaiveServer.Username != "u1" || s.NaiveServer.Password != "p1" {
		t.Error("naive credentials were not carried through")
	}
	if selectedTransport(s) != "naive" {
		t.Errorf("selectedTransport = %q, want naive", selectedTransport(s))
	}
}

// Empty generatable fields are filled, not refused.
func TestWebManagedNaiveServerGeneratesSecrets(t *testing.T) {
	requireLinux(t)
	n := managedWebFixture(t, "server", "naive")
	n.Managed.Username, n.Managed.Password = "", ""

	s, err := specFromNew(n)
	if err != nil {
		t.Fatalf("specFromNew: %v", err)
	}
	if s.NaiveServer.Password == "" {
		t.Error("empty password was not generated")
	}
	if s.NaiveServer.Username == "" {
		t.Error("empty username was not defaulted")
	}
}

// The xhttp client must bring the values only Iran knows.
func TestWebManagedXHTTPClientRequiresIranValues(t *testing.T) {
	requireLinux(t)
	n := managedWebFixture(t, "client", "xhttp")
	n.Managed.ServerName = "example.com"

	if _, err := specFromNew(n); err == nil {
		t.Fatal("xhttp client without uuid/path was accepted")
	}
	n.Managed.UUID = "6bf7a33e-7833-4e72-9219-506585657345"
	n.Managed.Path = "/secret"
	s, err := specFromNew(n)
	if err != nil {
		t.Fatalf("specFromNew: %v", err)
	}
	if !s.XrayClient.Enabled() || s.XrayClient.Mode != "xhttp" {
		t.Error("XrayClient was not configured for xhttp")
	}
	if s.RemoteAddr != "127.0.0.1:13080" {
		t.Errorf("RemoteAddr = %q, want 127.0.0.1:13080", s.RemoteAddr)
	}
}

// A reality server gets a full generated identity when the form leaves the
// fields empty.
func TestWebManagedRealityServerGeneratesIdentity(t *testing.T) {
	requireLinux(t)
	n := managedWebFixture(t, "server", "reality")
	n.Managed.ServerName = "cover.example.com"
	n.Managed.Target = "cover.example.com:443"

	s, err := specFromNew(n)
	if err != nil {
		t.Fatalf("specFromNew: %v", err)
	}
	x := s.XrayServer
	if !x.Enabled() || x.Mode != "reality" {
		t.Fatal("XrayServer was not configured for reality")
	}
	if x.UUID == "" || x.PrivateKey == "" || x.ShortID == "" {
		t.Error("reality identity was not generated")
	}
	if x.Target != "cover.example.com:443" {
		t.Errorf("Target = %q", x.Target)
	}
}

// Bad forms are refused before anything is written.
func TestWebManagedRefusesBadForms(t *testing.T) {
	requireLinux(t)
	base := func() NewTunnel { return managedWebFixture(t, "server", "naive") }
	for _, tc := range []struct {
		why    string
		mutate func(*NewTunnel)
	}{
		{"nil managed block", func(n *NewTunnel) { n.Managed = nil }},
		{"unknown managed transport", func(n *NewTunnel) { n.Transport = "carrier-pigeon" }},
		{"internal port equals public port", func(n *NewTunnel) { n.Managed.Internal = "4463" }},
		{"bad internal port", func(n *NewTunnel) { n.Managed.Internal = "99999" }},
		{"missing helper binary", func(n *NewTunnel) { n.Managed.Binary = "/nonexistent/helper" }},
		{"reality without target", func(n *NewTunnel) {
			n.Transport = "reality"
			n.Managed.ServerName = "cover.example.com"
		}},
		{"reality with bad target", func(n *NewTunnel) {
			n.Transport = "reality"
			n.Managed.ServerName = "cover.example.com"
			n.Managed.Target = "not-a-hostport"
		}},
		{"xhttp without server name", func(n *NewTunnel) { n.Transport = "xhttp" }},
	} {
		n := base()
		tc.mutate(&n)
		if _, err := specFromNew(n); err == nil {
			t.Errorf("%s was accepted", tc.why)
		}
	}
}

// The settings endpoint describes the managed transport without secrets.
func TestManagedInfoOfHidesSecrets(t *testing.T) {
	requireLinux(t)
	n := managedWebFixture(t, "server", "naive")
	n.Managed.Password = "super-secret"
	s, err := specFromNew(n)
	if err != nil {
		t.Fatalf("specFromNew: %v", err)
	}
	info := managedInfoOf(s)
	if info == nil {
		t.Fatal("managedInfoOf returned nil for a managed tunnel")
	}
	if info.Transport != "naive" {
		t.Errorf("Transport = %q, want naive", info.Transport)
	}
	if info.Internal != "13080" {
		t.Errorf("Internal = %q, want 13080", info.Internal)
	}
	// No password field exists on ManagedInfo by construction; the naive
	// native spec must still read tcp so nothing else mislabels it.
	if s.Transport != "tcp" {
		t.Errorf("carrier = %q, want tcp", s.Transport)
	}

	plain := TunnelSpec{Role: "server", Transport: "tcp", Name: "plain"}
	if managedInfoOf(plain) != nil {
		t.Error("managedInfoOf returned non-nil for a native transport")
	}
}

// A reality client round-trips the server's generated identity.
func TestWebManagedRealityClientRoundTrip(t *testing.T) {
	requireLinux(t)
	srv := managedWebFixture(t, "server", "reality")
	srv.Managed.ServerName = "cover.example.com"
	srv.Managed.Target = "cover.example.com:443"
	ss, err := specFromNew(srv)
	if err != nil {
		t.Fatalf("server specFromNew: %v", err)
	}
	_, pub, err := managedRealityKey(ss.XrayServer.PrivateKey)
	if err != nil {
		t.Fatalf("managedRealityKey: %v", err)
	}

	cli := managedWebFixture(t, "client", "reality")
	cli.Managed.ServerName = "cover.example.com"
	cli.Managed.UUID = ss.XrayServer.UUID
	cli.Managed.ShortID = ss.XrayServer.ShortID
	cli.Managed.PublicKey = pub
	cs, err := specFromNew(cli)
	if err != nil {
		t.Fatalf("client specFromNew: %v", err)
	}
	if cs.XrayClient.PublicKey != pub {
		t.Error("client did not carry the server's public key")
	}
	var _ = config.XrayClientConfig{}
}

// The generated certificate path is cleaned up with the test.
func TestWebManagedCertAutoGenerate(t *testing.T) {
	requireLinux(t)
	// Use a temp dir for certs instead of /etc/simorgh (no root in CI).
	old := host.CertDir
	host.CertDir = t.TempDir()
	t.Cleanup(func() { host.CertDir = old })
	n := managedWebFixture(t, "server", "naive")
	n.Managed.Certificate, n.Managed.Key = "", ""
	s, err := specFromNew(n)
	if err != nil {
		t.Fatalf("specFromNew: %v", err)
	}
	cert, key := s.NaiveServer.Certificate, s.NaiveServer.Key
	if cert == "" || key == "" {
		t.Fatal("no certificate was generated")
	}
	t.Cleanup(func() {
		os.Remove(cert)
		os.Remove(key)
	})
	if _, err := os.Stat(cert); err != nil {
		t.Errorf("generated cert not on disk: %v", err)
	}
	_ = filepath.Base(cert)
}
