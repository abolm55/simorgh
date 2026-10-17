package manage

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/abolm55/simorgh/config"
	"github.com/abolm55/simorgh/internal/optimize"
)

// ManagedCarrier carries the HTTPS managed-transport settings from the web
// setup form: naive, xhttp and reality.
//
// It is the non-interactive twin of what the CLI wizard collects in
// setupManagedCarrier. A managed tunnel is a plain TCP reverse tunnel on an
// internal loopback port, wrapped by an external helper (sing-box/naiveproxy
// for naive, xray for xhttp/reality) that terminates the public HTTPS
// endpoint. Empty generatable fields (passwords, UUIDs, keys, short IDs) are
// filled automatically; anything the form cannot decide — the REALITY cover
// endpoint, a certificate when the operator has one — must be provided.
//
// Server role fills the server-side configs, client role the client-side
// ones; Public is the public HTTPS endpoint (listened on by the server,
// dialed by the client) and Internal is the loopback reverse-TCP port both
// ends must share.
type ManagedCarrier struct {
	// Internal is the loopback reverse-TCP port both ends share. Empty picks
	// a free one, like the CLI default.
	Internal string `json:"internal"`
	// Binary is the helper binary path; empty uses the default
	// managedHelperPath(tool).
	Binary string `json:"binary"`

	// naive:
	Username string `json:"username"`
	Password string `json:"password"`
	// Certificate/Key are the server's PEM chain; empty generates a
	// self-signed one (the client then needs the CA file).
	Certificate string `json:"certificate"`
	Key         string `json:"key"`
	// CAFile is the client's private trust anchor; empty uses normal TLS
	// verification.
	CAFile string `json:"caFile"`

	// xhttp / reality:
	UUID       string `json:"uuid"`
	ServerName string `json:"serverName"`
	// Path is the xhttp private path; empty generates one (server).
	Path string `json:"path"`
	// Host optionally overrides the xhttp HTTP host.
	Host string `json:"host"`

	// reality (server):
	PrivateKey string `json:"privateKey"` // empty = generate
	Target     string `json:"target"`     // REALITY cover endpoint host:port
	// reality (client):
	PublicKey string `json:"publicKey"`
	// ShortID is shared; empty generates one (server).
	ShortID string `json:"shortId"`
}

// specFromManaged builds a tunnel spec for a managed HTTPS transport from the
// web form. It mirrors setupManagedCarrier: the carrier is plain TCP on an
// internal loopback port, and the helper configuration carries the HTTPS
// endpoint.
func specFromManaged(n NewTunnel, s TunnelSpec) (TunnelSpec, error) {
	if runtime.GOOS != "linux" {
		return s, fmt.Errorf("managed HTTPS transports require Linux")
	}
	m := n.Managed
	if m == nil {
		return s, fmt.Errorf("transport %q needs its HTTPS settings", n.Transport)
	}
	chosen := strings.ToLower(strings.TrimSpace(n.Transport))

	s.Name = strings.TrimSpace(n.Name)
	if !validName(s.Name) {
		return s, fmt.Errorf("invalid name %q — use letters, digits, dots and dashes (max 40)", n.Name)
	}

	bind, err := parseTunnelBind(n.TunnelPort)
	if err != nil {
		return s, fmt.Errorf("the public HTTPS port is not valid: %w", err)
	}
	publicPort := bind.Port

	s.Token = strings.TrimSpace(n.Token)
	if s.Token == "" {
		return s, fmt.Errorf("a security token is required — both ends must use the same one")
	}

	// Internal loopback port both ends share. It must differ from the public
	// port, like the CLI enforces.
	internal := strings.TrimSpace(m.Internal)
	if internal == "" {
		internal = freeInternalPort(publicPort)
		if internal == "" {
			return s, fmt.Errorf("could not find a free internal port — set one explicitly")
		}
	}
	if !validPort(internal) {
		return s, fmt.Errorf("the internal port %q is not valid", m.Internal)
	}
	if internal == publicPort {
		return s, fmt.Errorf("the internal port must differ from the public HTTPS port")
	}

	// Reset to a plain internal TCP reverse tunnel, exactly as the CLI does:
	// the helper owns everything the wire can see.
	s.Transport = "tcp"
	s.AcceptUDP = false
	s.FallbackAddrs, s.FallbackTransports = nil, nil

	var public string
	if s.Role == "server" {
		s.BindAddr = bind.Addr(n.IPv6)
		public = s.BindAddr
		s.Ports = parsePorts(n.Ports)
		if len(s.Ports) == 0 {
			return s, fmt.Errorf("at least one forwarded port is required")
		}
		if err := validatePortSpecs(s.Ports); err != nil {
			return s, err
		}
	} else {
		if bind.HasHost() {
			return s, fmt.Errorf("a client binds nothing — its tunnel port is the port on the server, so it takes a port alone")
		}
		host := strings.Trim(strings.TrimSpace(n.ServerAddr), "[]")
		if host == "" {
			return s, fmt.Errorf("the server address is required")
		}
		s.RemoteAddr = net.JoinHostPort(host, publicPort)
		public = s.RemoteAddr
	}

	// The clash check runs against the public endpoint, which is what the
	// helper will actually bind or dial.
	if why := portClash(s.Role, public, s.Name); why != "" {
		return s, fmt.Errorf("%s", why)
	}

	clearManagedTransport(&s)
	if s.Role == "server" {
		s.BindAddr = net.JoinHostPort("127.0.0.1", internal)
	} else {
		s.RemoteAddr = net.JoinHostPort("127.0.0.1", internal)
	}

	switch chosen {
	case "naive":
		if err := applyManagedNaive(&s, m, public); err != nil {
			return s, err
		}
	case "xhttp":
		if err := applyManagedXHTTP(&s, m, public); err != nil {
			return s, err
		}
	case "reality":
		if err := applyManagedReality(&s, m, public); err != nil {
			return s, err
		}
	default:
		return s, fmt.Errorf("unknown managed transport %q", n.Transport)
	}

	if err := validateManagedSpec(s); err != nil {
		return s, err
	}
	if why := managedEndpointClash(s); why != "" {
		return s, fmt.Errorf("%s", why)
	}
	optimize.ApplyQuiet(ReservedPorts())
	return s, nil
}

// randomShortID generates a REALITY short ID: exactly 16 hexadecimal
// characters (8 random bytes), as the CLI does.
func randomShortID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// freeInternalPort picks a free loopback port for the internal reverse
// tunnel, preferring 3080 like the CLI default.
func freeInternalPort(publicPort string) string {
	if publicPort != "3080" && !portHeld("127.0.0.1:3080") {
		return "3080"
	}
	for i := 0; i < 200; i++ {
		p := strconv.Itoa(SuggestPort())
		if p == "0" || p == publicPort {
			continue
		}
		if !portHeld("127.0.0.1:" + p) {
			return p
		}
	}
	return ""
}

// managedBinary resolves the helper binary path and checks it is executable,
// failing early with a clear message instead of a tunnel that will not start.
func managedBinary(tool, override string) (string, error) {
	path := strings.TrimSpace(override)
	if path == "" {
		path = managedHelperPath(tool)
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		return "", fmt.Errorf("helper %q is not executable at %s — install it or set its path", tool, path)
	}
	return path, nil
}

// managedServerCert returns the certificate/key pair for a managed server:
// the provided files when given, otherwise a fresh self-signed pair for the
// public endpoint's host — the CLI's "Generate Private Certificate" choice.
func managedServerCert(tunnelName, public, cert, key string) (string, string, error) {
	cert = strings.TrimSpace(cert)
	key = strings.TrimSpace(key)
	if cert != "" || key != "" {
		if cert == "" || key == "" {
			return "", "", fmt.Errorf("a certificate and its key must be given together")
		}
		if _, err := os.Stat(cert); err != nil {
			return "", "", fmt.Errorf("cannot read certificate %s: %w", cert, err)
		}
		if _, err := os.Stat(key); err != nil {
			return "", "", fmt.Errorf("cannot read key %s: %w", key, err)
		}
		return cert, key, nil
	}
	host := publicHost(public)
	cert, key, err := EnsureSelfSignedCert(tunnelName+"-https", host)
	if err != nil {
		return "", "", fmt.Errorf("could not generate a certificate: %w", err)
	}
	return cert, key, nil
}

// publicHost extracts the host part of a public endpoint for certificate
// generation; a wildcard becomes the machine's public address when known.
func publicHost(public string) string {
	host := addrHost(public, "")
	if host == "" || host == "0.0.0.0" || host == "::" {
		if ip := PublicIPv4(); ip != "" && ip != "-" {
			return ip
		}
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return host
}

func applyManagedNaive(s *TunnelSpec, m *ManagedCarrier, public string) error {
	username := strings.TrimSpace(m.Username)
	if username == "" {
		username = "simorgh"
	}
	if s.Role == "server" {
		binary, err := managedBinary("sing-box", m.Binary)
		if err != nil {
			return err
		}
		cert, key, err := managedServerCert(s.Name, public, m.Certificate, m.Key)
		if err != nil {
			return err
		}
		password := strings.TrimSpace(m.Password)
		if password == "" {
			password = randomToken(32)
		}
		s.NaiveServer = config.NaiveServerConfig{
			Binary: binary, Listen: public, Username: username,
			Password: password, Certificate: cert, Key: key,
		}
		return nil
	}
	binary, err := managedBinary("naive", m.Binary)
	if err != nil {
		return err
	}
	password := strings.TrimSpace(m.Password)
	if password == "" {
		return fmt.Errorf("the naive password is required — it must match the Iran server")
	}
	caFile := strings.TrimSpace(m.CAFile)
	if caFile != "" {
		if _, err := os.Stat(caFile); err != nil {
			return fmt.Errorf("cannot read CA file %s: %w", caFile, err)
		}
	}
	s.NaiveClient = config.NaiveClientConfig{
		Binary: binary, Server: public, Username: username,
		Password: password, CAFile: caFile,
	}
	return nil
}

func applyManagedXHTTP(s *TunnelSpec, m *ManagedCarrier, public string) error {
	binary, err := managedBinary("xray", m.Binary)
	if err != nil {
		return err
	}
	serverName := strings.TrimSpace(m.ServerName)
	if serverName == "" {
		return fmt.Errorf("the TLS server name is required")
	}
	if s.Role == "server" {
		uuid := strings.TrimSpace(m.UUID)
		if uuid == "" {
			var err error
			uuid, err = managedUUID()
			if err != nil {
				return err
			}
		}
		path := strings.TrimSpace(m.Path)
		if path == "" {
			path = "/" + randomToken(24)
		}
		cert, key, err := managedServerCert(s.Name, public, m.Certificate, m.Key)
		if err != nil {
			return err
		}
		s.XrayServer = config.XrayServerConfig{
			Binary: binary, Listen: public, Mode: "xhttp", UUID: uuid,
			ServerName: serverName, Path: path, Host: strings.TrimSpace(m.Host),
			Certificate: cert, Key: key,
		}
		return nil
	}
	for field, value := range map[string]string{"uuid": m.UUID, "server name": m.ServerName, "path": m.Path} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("the %s is required — it must match the Iran server", field)
		}
	}
	caFile := strings.TrimSpace(m.CAFile)
	if caFile != "" {
		if _, err := os.Stat(caFile); err != nil {
			return fmt.Errorf("cannot read CA file %s: %w", caFile, err)
		}
	}
	s.XrayClient = config.XrayClientConfig{
		Binary: binary, Server: public, Mode: "xhttp", UUID: strings.TrimSpace(m.UUID),
		ServerName: serverName, Path: strings.TrimSpace(m.Path),
		Host: strings.TrimSpace(m.Host), CAFile: caFile,
	}
	return nil
}

func applyManagedReality(s *TunnelSpec, m *ManagedCarrier, public string) error {
	binary, err := managedBinary("xray", m.Binary)
	if err != nil {
		return err
	}
	serverName := strings.TrimSpace(m.ServerName)
	if serverName == "" {
		return fmt.Errorf("the SNI server name is required")
	}
	if s.Role == "server" {
		target := strings.TrimSpace(m.Target)
		if target == "" {
			return fmt.Errorf("a REALITY cover endpoint (target host:port) is required")
		}
		if _, _, err := net.SplitHostPort(target); err != nil {
			return fmt.Errorf("the REALITY target %q is not a valid host:port", m.Target)
		}
		uuid := strings.TrimSpace(m.UUID)
		if uuid == "" {
			var err error
			uuid, err = managedUUID()
			if err != nil {
				return err
			}
		}
		privateKey, _, err := managedRealityKey(strings.TrimSpace(m.PrivateKey))
		if err != nil {
			return err
		}
		shortID := strings.TrimSpace(m.ShortID)
		if shortID == "" {
			var err error
			shortID, err = randomShortID()
			if err != nil {
				return err
			}
		}
		s.XrayServer = config.XrayServerConfig{
			Binary: binary, Listen: public, Mode: "reality", UUID: uuid,
			ServerName: serverName, PrivateKey: privateKey, Target: target,
			ShortID: shortID,
		}
		return nil
	}
	for field, value := range map[string]string{
		"uuid": m.UUID, "SNI": m.ServerName, "short ID": m.ShortID, "public key": m.PublicKey,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("the %s is required — it must match the Iran server", field)
		}
	}
	s.XrayClient = config.XrayClientConfig{
		Binary: binary, Server: public, Mode: "reality", UUID: strings.TrimSpace(m.UUID),
		ServerName: serverName, PublicKey: strings.TrimSpace(m.PublicKey),
		ShortID: strings.TrimSpace(m.ShortID),
	}
	return nil
}
