// Package tlsconfig compiles frontend TLS configuration from one owned
// configuration generation. The compiled snapshot has no Store or filesystem
// dependency and its selectors never expose their retained certificate state.
package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/resource"
)

var tls12CipherSuites = map[string]uint16{
	"ECDHE-ECDSA-AES128-GCM-SHA256": tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	"ECDHE-RSA-AES128-GCM-SHA256":   tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	"ECDHE-ECDSA-AES256-GCM-SHA384": tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	"ECDHE-RSA-AES256-GCM-SHA384":   tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	"ECDHE-ECDSA-CHACHA20-POLY1305": tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	"ECDHE-RSA-CHACHA20-POLY1305":   tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
}

var tls13CipherNames = map[string]struct{}{
	"TLS_AES_128_GCM_SHA256":       {},
	"TLS_AES_256_GCM_SHA384":       {},
	"TLS_CHACHA20_POLY1305_SHA256": {},
	"TLS_AES_128_CCM_SHA256":       {},
	"TLS_AES_128_CCM_8_SHA256":     {},
}

// Input is the complete frontend TLS input for one generation. Config and
// SSLs are consumed during Compile and are not retained. TrustedClientCAPEM is
// the already-read content named by ssl_trusted_certificate, keeping file I/O
// with the caller that owns static configuration loading.
type Input struct {
	Config             *config.Config
	SSLs               map[string]resource.SSL
	TrustedClientCAPEM []byte
}

// BaseInput is the static frontend TLS input shared by immutable generation
// compilation and the legacy server wrapper. It deliberately contains no SSL
// resource index or certificate-selection authority.
type BaseInput struct {
	Config             *config.Config
	TrustedClientCAPEM []byte
}

// Snapshot owns a compiled frontend TLS configuration and certificate index.
type Snapshot struct {
	tlsConfig *tls.Config
}

// FrontendEnabled reports whether the static configuration owns at least one
// usable frontend TLS listener. TLS-only files and resource material are not
// authoritative when this returns false.
func FrontendEnabled(cfg *config.Config) bool {
	if cfg == nil || !cfg.Apisix.Ssl.Enable {
		return false
	}
	for _, listener := range cfg.Apisix.Ssl.Listen {
		if listener.Port >= 1 && listener.Port <= 65535 {
			return true
		}
	}
	return false
}

// Compile validates and compiles frontend TLS settings and active server SSL
// resources into an immutable snapshot.
func Compile(input Input) (*Snapshot, error) {
	base, settings, err := compileBase(BaseInput{
		Config: input.Config, TrustedClientCAPEM: input.TrustedClientCAPEM,
	})
	if err != nil {
		return nil, err
	}
	index, err := compileCertificateIndex(input.SSLs)
	if err != nil {
		return nil, err
	}

	selectorBase := cloneTLSConfig(base)
	base.GetCertificate = index.certificateSelector(settings.fallbackSNI)
	base.GetConfigForClient = index.configSelector(selectorBase, settings.fallbackSNI)
	return &Snapshot{tlsConfig: base}, nil
}

// CompileBase validates and compiles only the static frontend TLS settings.
// The returned config has no certificate callbacks, allowing the legacy server
// wrapper to retain Store-backed SNI selection during migration.
func CompileBase(input BaseInput) (*Snapshot, error) {
	base, _, err := compileBase(input)
	if err != nil {
		return nil, err
	}
	return &Snapshot{tlsConfig: base}, nil
}

func compileBase(input BaseInput) (*tls.Config, compiledSettings, error) {
	settings := frontendSettings(input.Config)
	minVersion, maxVersion, err := parseProtocols(settings.protocols, settings.enabled)
	if err != nil {
		return nil, compiledSettings{}, fmt.Errorf("frontend TLS protocols: %w", err)
	}
	cipherSuites, err := parseCipherSuites(settings.ciphers, minVersion, settings.enabled)
	if err != nil {
		return nil, compiledSettings{}, fmt.Errorf("frontend TLS ciphers: %w", err)
	}

	clientCAs, err := compileTrustedClientCAs(settings.trustedCertificate, input.TrustedClientCAPEM)
	if err != nil {
		return nil, compiledSettings{}, err
	}

	base := &tls.Config{
		MinVersion:             minVersion,
		MaxVersion:             maxVersion,
		CipherSuites:           slices.Clone(cipherSuites),
		SessionTicketsDisabled: !settings.sessionTickets,
		NextProtos:             []string{"http/1.1"},
		ClientCAs:              cloneCertPool(clientCAs),
	}
	if settings.http2 {
		base.NextProtos = []string{"h2", "http/1.1"}
	}
	if clientCAs != nil {
		base.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return base, settings, nil
}

// TLSConfig returns a defensive clone backed by immutable selectors. Mutating
// the returned config, selected certificates, or CA pools cannot affect later
// calls.
func (snapshot *Snapshot) TLSConfig() *tls.Config {
	if snapshot == nil || snapshot.tlsConfig == nil {
		return nil
	}
	return cloneTLSConfig(snapshot.tlsConfig)
}

type compiledSettings struct {
	enabled            bool
	protocols          string
	ciphers            string
	sessionTickets     bool
	fallbackSNI        string
	trustedCertificate string
	http2              bool
}

func frontendSettings(cfg *config.Config) compiledSettings {
	if cfg == nil {
		return compiledSettings{}
	}
	ssl := cfg.Apisix.Ssl
	settings := compiledSettings{
		enabled:            ssl.Enable,
		protocols:          ssl.SslProtocols,
		ciphers:            ssl.SslCiphers,
		sessionTickets:     ssl.SslSessionTickets,
		fallbackSNI:        strings.TrimSpace(ssl.FallbackSNI),
		trustedCertificate: strings.TrimSpace(ssl.SslTrustedCertificate),
		http2:              cfg.Apisix.EnableHttp2,
	}
	return settings
}

func parseProtocols(raw string, required bool) (uint16, uint16, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if required {
			return 0, 0, fmt.Errorf("protocol list must not be empty when TLS is enabled")
		}
		return tls.VersionTLS12, 0, nil
	}

	seen := make(map[string]struct{}, 2)
	var minVersion, maxVersion uint16
	for token := range strings.FieldsSeq(raw) {
		if _, exists := seen[token]; exists {
			return 0, 0, fmt.Errorf("duplicate protocol %q", token)
		}
		seen[token] = struct{}{}
		var version uint16
		switch token {
		case "TLSv1.2":
			version = tls.VersionTLS12
		case "TLSv1.3":
			version = tls.VersionTLS13
		default:
			return 0, 0, fmt.Errorf("unsupported protocol %q", token)
		}
		if minVersion == 0 || version < minVersion {
			minVersion = version
		}
		if version > maxVersion {
			maxVersion = version
		}
	}
	if minVersion == 0 {
		return 0, 0, fmt.Errorf("protocol list must not be empty")
	}
	return minVersion, maxVersion, nil
}

func parseCipherSuites(raw string, minVersion uint16, required bool) ([]uint16, error) {
	if strings.TrimSpace(raw) == "" {
		if required && minVersion == tls.VersionTLS12 {
			return nil, fmt.Errorf("TLS 1.2 requires a non-empty cipher list")
		}
		return nil, nil
	}
	if minVersion == tls.VersionTLS13 {
		return nil, fmt.Errorf("TLS 1.3-only configuration must not set TLS 1.2 cipher suites")
	}

	parts := strings.Split(raw, ":")
	result := make([]uint16, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, fmt.Errorf("cipher list contains an empty segment")
		}
		if _, exists := tls13CipherNames[name]; exists {
			return nil, fmt.Errorf("TLS 1.3 cipher suite %q cannot be configured", name)
		}
		cipherSuite, exists := tls12CipherSuites[name]
		if !exists {
			return nil, fmt.Errorf("unknown or unsupported cipher suite %q", name)
		}
		result = append(result, cipherSuite)
	}
	return result, nil
}

func compileTrustedClientCAs(configuredPath string, certificatePEM []byte) (*x509.CertPool, error) {
	if len(certificatePEM) == 0 {
		if configuredPath != "" {
			return nil, fmt.Errorf("frontend TLS trusted client CA material was not provided for %q", configuredPath)
		}
		return nil, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(slices.Clone(certificatePEM)) {
		return nil, fmt.Errorf("frontend TLS trusted client CA contains no certificates")
	}
	return pool, nil
}

type certificateIndex struct {
	exact    map[string]certificateEntry
	wildcard []wildcardCertificateEntry
}

type certificateEntry struct {
	id          string
	certificate tls.Certificate
	clientCAs   *x509.CertPool
	clientDepth int
}

type wildcardCertificateEntry struct {
	certificateEntry
	suffix string
}

func compileCertificateIndex(ssls map[string]resource.SSL) (*certificateIndex, error) {
	index := &certificateIndex{exact: make(map[string]certificateEntry)}
	owners := make(map[string]string)
	for _, id := range slices.Sorted(maps.Keys(ssls)) {
		ssl := cloneSSL(ssls[id])
		if ssl.Status == 0 {
			continue
		}
		if ssl.Status != 1 {
			return nil, fmt.Errorf("frontend TLS SSL resource %q has unsupported status %d", id, ssl.Status)
		}
		if ssl.Type == "client" {
			continue
		}
		if ssl.Type != "" && ssl.Type != "server" {
			return nil, fmt.Errorf("frontend TLS SSL resource %q has unsupported type %q", id, ssl.Type)
		}
		certificate, err := tls.X509KeyPair([]byte(ssl.Cert), []byte(ssl.Key))
		if err != nil {
			return nil, fmt.Errorf("frontend TLS SSL resource %q load certificate: %w", id, err)
		}
		clientCAs, clientDepth, err := compileResourceClientCAs(ssl.Client)
		if err != nil {
			return nil, fmt.Errorf("frontend TLS SSL resource %q: %w", id, err)
		}
		entry := certificateEntry{
			id: id, certificate: certificate, clientCAs: clientCAs, clientDepth: clientDepth,
		}
		for _, rawSNI := range sslSNIs(ssl) {
			sni := normalizeSNI(rawSNI)
			if sni == "" {
				continue
			}
			if owner, exists := owners[sni]; exists {
				return nil, fmt.Errorf(
					"frontend TLS duplicate SNI %q is owned by SSL resources %q and %q",
					sni,
					owner,
					id,
				)
			}
			owners[sni] = id
			if strings.HasPrefix(sni, "*.") {
				index.wildcard = append(index.wildcard, wildcardCertificateEntry{
					certificateEntry: entry, suffix: sni[1:],
				})
				continue
			}
			index.exact[sni] = entry
		}
	}
	return index, nil
}

func compileResourceClientCAs(client *resource.SSLClient) (*x509.CertPool, int, error) {
	if client == nil {
		return nil, 0, nil
	}
	if client.Depth != 1 {
		return nil, 0, fmt.Errorf("unsupported SSL client depth %d; only depth 1 is supported", client.Depth)
	}
	if len(client.SkipMTLSURIRegex) > 0 {
		return nil, 0, fmt.Errorf("unsupported SSL client skip_mtls_uri_regex")
	}
	ca := strings.TrimSpace(client.CA)
	if ca == "" {
		return nil, 0, fmt.Errorf("SSL client.ca must not be empty")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(ca)) {
		return nil, 0, fmt.Errorf("SSL client.ca contains no certificates")
	}
	return pool, client.Depth, nil
}

func (index *certificateIndex) certificateSelector(
	fallbackSNI string,
) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		entry, err := index.selectEntry(serverName(hello, fallbackSNI))
		if err != nil {
			return nil, err
		}
		certificate := cloneCertificate(entry.certificate)
		return &certificate, nil
	}
}

func (index *certificateIndex) configSelector(
	base *tls.Config,
	fallbackSNI string,
) func(*tls.ClientHelloInfo) (*tls.Config, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		entry, err := index.selectEntry(serverName(hello, fallbackSNI))
		if err != nil {
			return nil, err
		}
		certificate := cloneCertificate(entry.certificate)
		selected := cloneTLSConfig(base)
		selected.Certificates = []tls.Certificate{certificate}
		if entry.clientCAs != nil {
			selected.ClientCAs = entry.clientCAs.Clone()
			selected.ClientAuth = tls.RequireAndVerifyClientCert
			enforceClientCertificateDepth(selected, entry.clientDepth)
		}
		return selected, nil
	}
}

func (index *certificateIndex) selectEntry(serverName string) (certificateEntry, error) {
	normalized := normalizeSNI(serverName)
	if entry, exists := index.exact[normalized]; exists {
		return entry, nil
	}
	for _, entry := range index.wildcard {
		if wildcardMatches(normalized, entry.suffix) {
			return entry.certificateEntry, nil
		}
	}
	return certificateEntry{}, fmt.Errorf("no SSL certificate for SNI %q", strings.TrimSpace(serverName))
}

func serverName(hello *tls.ClientHelloInfo, fallbackSNI string) string {
	if hello != nil {
		if name := strings.TrimSpace(hello.ServerName); name != "" {
			return name
		}
	}
	return strings.TrimSpace(fallbackSNI)
}

func normalizeSNI(sni string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(sni), "."))
}

func wildcardMatches(serverName, suffix string) bool {
	if !strings.HasSuffix(serverName, suffix) {
		return false
	}
	prefix := strings.TrimSuffix(serverName, suffix)
	return prefix != "" && !strings.Contains(prefix, ".")
}

func sslSNIs(ssl resource.SSL) []string {
	if len(ssl.Snis) > 0 {
		return ssl.Snis
	}
	if ssl.Sni != "" {
		return []string{ssl.Sni}
	}
	return nil
}

func cloneSSL(ssl resource.SSL) resource.SSL {
	ssl.Snis = slices.Clone(ssl.Snis)
	ssl.Labels = maps.Clone(ssl.Labels)
	if ssl.Client != nil {
		client := *ssl.Client
		client.SkipMTLSURIRegex = slices.Clone(ssl.Client.SkipMTLSURIRegex)
		ssl.Client = &client
	}
	return ssl
}

func enforceClientCertificateDepth(tlsConfig *tls.Config, maximumDepth int) {
	previous := tlsConfig.VerifyConnection
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if previous != nil {
			if err := previous(state); err != nil {
				return err
			}
		}
		for _, chain := range state.VerifiedChains {
			if len(chain)-1 <= maximumDepth {
				return nil
			}
		}
		return fmt.Errorf("client certificate chain exceeds verification depth %d", maximumDepth)
	}
}

func cloneTLSConfig(source *tls.Config) *tls.Config {
	if source == nil {
		return nil
	}
	cloned := source.Clone()
	cloned.CipherSuites = slices.Clone(source.CipherSuites)
	cloned.NextProtos = slices.Clone(source.NextProtos)
	cloned.ClientCAs = cloneCertPool(source.ClientCAs)
	cloned.RootCAs = cloneCertPool(source.RootCAs)
	return cloned
}

func cloneCertPool(pool *x509.CertPool) *x509.CertPool {
	if pool == nil {
		return nil
	}
	return pool.Clone()
}

func cloneCertificate(source tls.Certificate) tls.Certificate {
	return tls.Certificate{
		Certificate:                  cloneByteSlices(source.Certificate),
		PrivateKey:                   source.PrivateKey,
		SupportedSignatureAlgorithms: slices.Clone(source.SupportedSignatureAlgorithms),
		OCSPStaple:                   slices.Clone(source.OCSPStaple),
		SignedCertificateTimestamps:  cloneByteSlices(source.SignedCertificateTimestamps),
	}
}

func cloneByteSlices(source [][]byte) [][]byte {
	if source == nil {
		return nil
	}
	cloned := make([][]byte, len(source))
	for index := range source {
		cloned[index] = slices.Clone(source[index])
	}
	return cloned
}
