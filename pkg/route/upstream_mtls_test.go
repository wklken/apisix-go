package route

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestNormalizeSSLID(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		want    string
		wantErr bool
	}{
		{name: "string", value: "ssl-1", want: "ssl-1"},
		{name: "number", value: float64(17), want: "17"},
		{name: "fraction", value: 1.5, wantErr: true},
		{name: "empty", value: " ", wantErr: true},
		{name: "unsupported", value: true, wantErr: true},
		{name: "json number", value: json.Number("3"), want: "3"},
		{name: "nan float", value: math.NaN(), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeSSLID(test.value)
			if (err != nil) != test.wantErr {
				t.Fatalf("normalizeSSLID(%#v) error = %v, wantErr %v", test.value, err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Fatalf("normalizeSSLID(%#v) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}

func TestBuildReverseHandlerRejectsInvalidUpstreamMTLSMaterialWithoutTargets(t *testing.T) {
	for _, scheme := range []string{"https", "grpcs"} {
		t.Run(scheme, func(t *testing.T) {
			effective := testEffectiveConfig()
			_, err := PlanRouteUpstream(
				resource.Route{Upstream: resource.Upstream{
					Scheme: scheme,
					TLS: &resource.UpstreamTLS{
						ClientCert: "not-a-certificate",
						ClientKey:  "not-a-key",
					},
				}},
				resource.Service{}, nil, nil, &effective.Config,
			)
			if err == nil {
				t.Fatalf("buildReverseHandler() error = nil, want invalid %s client certificate rejection", scheme)
			}
		})
	}
}

func TestBuildReverseHandlerRejectsPlaintextUpstreamClientCertificate(t *testing.T) {
	for _, scheme := range []string{"http", "grpc"} {
		t.Run(scheme, func(t *testing.T) {
			effective := testEffectiveConfig()
			_, err := PlanRouteUpstream(
				resource.Route{Upstream: resource.Upstream{
					Scheme: scheme,
					TLS: &resource.UpstreamTLS{
						ClientCert: "configured",
						ClientKey:  "configured",
					},
				}},
				resource.Service{}, nil, nil, &effective.Config,
			)
			if err == nil {
				t.Fatalf("buildReverseHandler() error = nil, want plaintext %s client certificate rejection", scheme)
			}
		})
	}
}

func TestBuildReverseHandlerHTTPSUpstreamMTLSHandshake(t *testing.T) {
	testReverseHandlerUpstreamMTLSHandshake(t, "https", false)
}

func TestBuildReverseHandlerGRPCSUpstreamMTLSHandshakeUsesHTTP2(t *testing.T) {
	testReverseHandlerUpstreamMTLSHandshake(t, "grpcs", true)
}

func testReverseHandlerUpstreamMTLSHandshake(t *testing.T, scheme string, wantHTTP2 bool) {
	t.Helper()
	serverCertificate, clientCertificate, clientKey, clientCAs := routeMTLSCertificates(t)
	protocol := make(chan string, 1)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol <- r.Proto
		w.WriteHeader(http.StatusNoContent)
	}))
	upstream.EnableHTTP2 = wantHTTP2
	upstream.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	upstream.StartTLS()
	defer upstream.Close()

	parsed, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	handler := testPreparedProxyHandler(t, resource.Route{Upstream: resource.Upstream{
		Scheme: scheme,
		Nodes:  []resource.Node{{Host: parsed.Hostname(), Port: port, Weight: 1}},
		TLS: &resource.UpstreamTLS{
			ClientCert: clientCertificate,
			ClientKey:  clientKey,
			Verify:     false,
		},
	}}, resource.Service{}, testEffectiveConfig())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://route.test/mtls", nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf(
			"route response status = %d, want %d; body=%q",
			recorder.Code,
			http.StatusNoContent,
			recorder.Body.String(),
		)
	}
	select {
	case got := <-protocol:
		if wantHTTP2 && got != "HTTP/2.0" {
			t.Fatalf("upstream protocol = %q, want HTTP/2.0", got)
		}
		if !wantHTTP2 && got != "HTTP/1.1" {
			t.Fatalf("upstream protocol = %q, want HTTP/1.1", got)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive the mTLS request")
	}
}

func TestBuildTransportOptionWithSSLResolverSupportsIDAndRejectsControls(t *testing.T) {
	_, clientCertificate, clientKey, _ := routeMTLSCertificates(t)
	resolver := func(id string) (resource.SSL, error) {
		if id != "ssl-1" {
			return resource.SSL{}, fmt.Errorf("unexpected SSL ID %q", id)
		}
		return resource.SSL{ID: id, Cert: clientCertificate, Key: clientKey, Status: 1}, nil
	}
	base := resource.Upstream{Scheme: "https", TLS: &resource.UpstreamTLS{
		ClientCertID: "ssl-1", Verify: true,
	}}
	if _, err := buildTransportOptionWithSSLResolver(
		resource.Route{},
		base,
		resolver,
	); err != nil {
		t.Fatalf("ID-based client certificate: %v", err)
	}

	tests := []struct {
		name     string
		upstream resource.Upstream
		wantErr  string
	}{
		{
			name: "conflicting inline and ID",
			upstream: resource.Upstream{Scheme: "https", TLS: &resource.UpstreamTLS{
				ClientCertID: "ssl-1", ClientCert: clientCertificate, ClientKey: clientKey,
			}},
			wantErr: "cannot be combined",
		},
		{
			name: "missing key",
			upstream: resource.Upstream{Scheme: "https", TLS: &resource.UpstreamTLS{
				ClientCert: clientCertificate,
			}},
			wantErr: "configured together",
		},
		{
			name: "disabled ID",
			upstream: resource.Upstream{Scheme: "https", TLS: &resource.UpstreamTLS{
				ClientCertID: "ssl-1",
			}},
			wantErr: "disabled",
		},
		{
			name: "empty ID material",
			upstream: resource.Upstream{Scheme: "https", TLS: &resource.UpstreamTLS{
				ClientCertID: "ssl-1",
			}},
			wantErr: "must contain",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolve := resolver
			switch test.name {
			case "disabled ID":
				resolve = func(string) (resource.SSL, error) {
					return resource.SSL{Cert: clientCertificate, Key: clientKey, Status: 0}, nil
				}
			case "empty ID material":
				resolve = func(string) (resource.SSL, error) {
					return resource.SSL{Status: 1}, nil
				}
			}
			_, err := buildTransportOptionWithSSLResolver(
				resource.Route{},
				test.upstream,
				resolve,
			)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("buildTransportOptionWithSSLResolver() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestBuildClusterConfigWithSSLResolverChangesOnRotation(t *testing.T) {
	firstCert, firstKey := routeLeafCertificate(t, "first-client")
	secondCert, secondKey := routeLeafCertificate(t, "second-client")
	resolver := func(id string) (resource.SSL, error) {
		if id == "first" {
			return resource.SSL{Cert: firstCert, Key: firstKey, Status: 1}, nil
		}
		return resource.SSL{Cert: secondCert, Key: secondKey, Status: 1}, nil
	}
	base := resource.Upstream{Scheme: "https", TLS: &resource.UpstreamTLS{ClientCertID: "first"}}
	first, err := buildClusterConfigWithSSLResolver(
		resource.Route{},
		base,
		map[string]int{"https://127.0.0.1:1": 1},
		resolver,
		&testEffectiveConfig().Config,
	)
	if err != nil {
		t.Fatalf("first cluster config: %v", err)
	}
	secondUpstream := base
	secondUpstream.TLS = &resource.UpstreamTLS{ClientCertID: "second"}
	second, err := buildClusterConfigWithSSLResolver(
		resource.Route{},
		secondUpstream,
		map[string]int{"https://127.0.0.1:1": 1},
		resolver,
		&testEffectiveConfig().Config,
	)
	if err != nil {
		t.Fatalf("rotated cluster config: %v", err)
	}
	firstKeyDigest, err := first.Key()
	if err != nil {
		t.Fatal(err)
	}
	secondKeyDigest, err := second.Key()
	if err != nil {
		t.Fatal(err)
	}
	if firstKeyDigest == secondKeyDigest {
		t.Fatal("rotated upstream certificate reused the same cluster key")
	}
}

func TestUpstreamMTLSSameIDRotationChangesPreparedClusterIdentity(t *testing.T) {
	caCertificate, caKey, _ := newRouteMTLSCertificateAuthority(t)
	clientACert, clientAKey := routeSignedCertificatePEM(
		t, caCertificate, caKey, 3, x509.ExtKeyUsageClientAuth, nil,
	)
	clientBCert, clientBKey := routeSignedCertificatePEM(
		t, caCertificate, caKey, 4, x509.ExtKeyUsageClientAuth, nil,
	)
	for _, scheme := range []string{"https", "grpcs"} {
		t.Run(scheme, func(t *testing.T) {
			routeResource := resource.Route{
				ID: "mtls-rotation",
				Upstream: resource.Upstream{
					Scheme: scheme,
					Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
					TLS:    &resource.UpstreamTLS{ClientCertID: "shared-client", Verify: false},
				},
			}
			plan := func(cert, key string) UpstreamPlan {
				t.Helper()
				planned, err := PlanRouteUpstream(
					routeResource,
					resource.Service{},
					nil,
					map[string]resource.SSL{"shared-client": {ID: "shared-client", Cert: cert, Key: key, Status: 1}},
					&testEffectiveConfig().Config,
				)
				if err != nil {
					t.Fatalf("PlanRouteUpstream() error = %v", err)
				}
				return planned
			}
			first := plan(clientACert, clientAKey)
			second := plan(clientBCert, clientBKey)
			third := plan(clientBCert, clientBKey)
			firstKey, err := first.ClusterConfig.Key()
			if err != nil {
				t.Fatal(err)
			}
			secondKey, err := second.ClusterConfig.Key()
			if err != nil {
				t.Fatal(err)
			}
			thirdKey, err := third.ClusterConfig.Key()
			if err != nil {
				t.Fatal(err)
			}
			if firstKey == secondKey {
				t.Fatal("same-ID certificate rotation reused the previous prepared cluster identity")
			}
			if secondKey != thirdKey {
				t.Fatal("identical rotated certificate did not reuse the prepared cluster identity")
			}
		})
	}
}

func routeMTLSCertificates(t *testing.T) (tls.Certificate, string, string, *x509.CertPool) {
	t.Helper()
	caCertificate, caKey, clientCAs := newRouteMTLSCertificateAuthority(t)
	server := routeSignedCertificate(t, caCertificate, caKey, 2, x509.ExtKeyUsageServerAuth, []string{"localhost"})
	client, clientKey := routeSignedCertificatePEM(t, caCertificate, caKey, 3, x509.ExtKeyUsageClientAuth, nil)
	return server, client, clientKey, clientCAs
}

func newRouteMTLSCertificateAuthority(t *testing.T) (*x509.Certificate, *rsa.PrivateKey, *x509.CertPool) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "route test CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(caCertificate)
	return caCertificate, caKey, clientCAs
}

func routeLeafCertificate(t *testing.T, commonName string) (string, string) {
	server, client, key, _ := routeMTLSCertificates(t)
	_ = server
	_ = commonName
	return client, key
}

func routeSignedCertificate(
	t *testing.T,
	caCertificate *x509.Certificate,
	caKey *rsa.PrivateKey,
	serial int64,
	extKeyUsage x509.ExtKeyUsage,
	dnsNames []string,
) tls.Certificate {
	certPEM, keyPEM := routeSignedCertificatePEM(t, caCertificate, caKey, serial, extKeyUsage, dnsNames)
	certificate, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		t.Fatalf("parse signed certificate: %v", err)
	}
	return certificate
}

func routeSignedCertificatePEM(
	t *testing.T,
	caCertificate *x509.Certificate,
	caKey *rsa.PrivateKey,
	serial int64,
	extKeyUsage x509.ExtKeyUsage,
	dnsNames []string,
) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "route test leaf"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{extKeyUsage},
		DNSNames:     dnsNames,
	}
	if len(dnsNames) > 0 {
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, caCertificate, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}
