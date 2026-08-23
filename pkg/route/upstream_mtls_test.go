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
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
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
			_, err := (NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())).buildReverseHandler(
				resource.Route{Upstream: resource.Upstream{
					Scheme: scheme,
					TLS: &resource.UpstreamTLS{
						ClientCert: "not-a-certificate",
						ClientKey:  "not-a-key",
					},
				}},
				resource.Service{},
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
			_, err := (NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())).buildReverseHandler(
				resource.Route{Upstream: resource.Upstream{
					Scheme: scheme,
					TLS: &resource.UpstreamTLS{
						ClientCert: "configured",
						ClientKey:  "configured",
					},
				}},
				resource.Service{},
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
	builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
	t.Cleanup(builder.Stop)
	handler, err := builder.buildReverseHandler(resource.Route{Upstream: resource.Upstream{
		Scheme: scheme,
		Nodes:  []resource.Node{{Host: parsed.Hostname(), Port: port, Weight: 1}},
		TLS: &resource.UpstreamTLS{
			ClientCert: clientCertificate,
			ClientKey:  clientKey,
			Verify:     false,
		},
	}}, resource.Service{})
	if err != nil {
		t.Fatalf("buildReverseHandler() error = %v", err)
	}
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
		&testEffectiveConfig().Config,
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
				&testEffectiveConfig().Config,
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

func TestUpstreamMTLSSameIDRotationRebuildsClusterUntilFinalLease(t *testing.T) {
	for _, test := range []struct {
		scheme    string
		wantProto string
	}{
		{scheme: "https", wantProto: "HTTP/1.1"},
		{scheme: "grpcs", wantProto: "HTTP/2.0"},
	} {
		t.Run(test.scheme, func(t *testing.T) {
			caCertificate, caKey, clientCAs := newRouteMTLSCertificateAuthority(t)
			serverCertificate := routeSignedCertificate(
				t,
				caCertificate,
				caKey,
				2,
				x509.ExtKeyUsageServerAuth,
				[]string{"localhost"},
			)
			clientACert, clientAKey := routeSignedCertificatePEM(
				t,
				caCertificate,
				caKey,
				3,
				x509.ExtKeyUsageClientAuth,
				nil,
			)
			clientBCert, clientBKey := routeSignedCertificatePEM(
				t,
				caCertificate,
				caKey,
				4,
				x509.ExtKeyUsageClientAuth,
				nil,
			)
			observed := make(chan struct {
				serial int64
				proto  string
			}, 3)
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed <- struct {
					serial int64
					proto  string
				}{serial: r.TLS.PeerCertificates[0].SerialNumber.Int64(), proto: r.Proto}
				w.WriteHeader(http.StatusNoContent)
			}))
			upstream.EnableHTTP2 = test.scheme == "grpcs"
			upstream.TLS = &tls.Config{
				Certificates: []tls.Certificate{serverCertificate},
				ClientCAs:    clientCAs,
				ClientAuth:   tls.RequireAndVerifyClientCert,
				MinVersion:   tls.VersionTLS12,
			}
			upstream.StartTLS()
			t.Cleanup(upstream.Close)
			parsed, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatalf("parse upstream URL: %v", err)
			}
			port, err := strconv.Atoi(parsed.Port())
			if err != nil {
				t.Fatalf("parse upstream port: %v", err)
			}

			events := make(chan *store.Event)
			storage, err := store.Open(t.TempDir()+"/rotation.db", events, testDataEncryptionService())
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			storage.Start()
			previousStore := store.ReplaceGlobalStoreForTest(storage)
			t.Cleanup(func() {
				store.ReplaceGlobalStoreForTest(previousStore)
				_ = storage.Stop()
			})
			reloadEvents := make(chan struct{}, 2)
			storage.AddEventUpdateHook(func(event *store.Event) {
				if strings.Contains(string(event.Key), "/ssls/") && store.IsHTTPRouteReloadBucket("ssls") {
					reloadEvents <- struct{}{}
				}
			})
			put := func(bucket, id string, value []byte) {
				event := store.NewEvent()
				event.Type = store.EventTypePut
				event.Key = []byte("/apisix/" + bucket + "/" + id)
				event.Value = value
				events <- event
			}
			putClientCertificate := func(cert, key string) {
				value, marshalErr := json.Marshal(resource.SSL{
					ID: "shared-client", Cert: cert, Key: key, Status: 1,
				})
				if marshalErr != nil {
					t.Fatalf("marshal SSL resource: %v", marshalErr)
				}
				put("ssls", "shared-client", value)
				if syncErr := storage.Sync(); syncErr != nil {
					t.Fatalf("sync SSL resource: %v", syncErr)
				}
				select {
				case <-reloadEvents:
				case <-time.After(time.Second):
					t.Fatal("SSL update did not schedule HTTP route reload")
				}
			}
			putClientCertificate(clientACert, clientAKey)
			put(
				"routes",
				"mtls-rotation",
				fmt.Appendf(
					nil,
					`{"id":"mtls-rotation","uri":"/rotate","upstream":{"scheme":"%s","nodes":[{"host":"%s","port":%d,"weight":1}],"tls":{"client_cert_id":"shared-client","verify":false}}}`,
					test.scheme,
					parsed.Hostname(),
					port,
				),
			)
			if err := storage.Sync(); err != nil {
				t.Fatalf("sync route: %v", err)
			}

			registry := pxy.NewClusterRegistry(pxy.NopClusterObserver{})
			t.Cleanup(registry.Close)
			buildAndRequest := func(wantSerial int64) *Builder {
				builder := NewBuilderWithClusterRegistry(
					storage,
					"127.0.0.1:9080",
					registry,
					testEffectiveConfig(),
					testDataEncryptionResolver(),
				)
				handler, buildErr := builder.BuildStrict()
				if buildErr != nil {
					t.Fatalf("BuildStrict() error = %v", buildErr)
				}
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://route.test/rotate", nil))
				if recorder.Code != http.StatusNoContent {
					t.Fatalf(
						"route status = %d, want %d; body=%q",
						recorder.Code,
						http.StatusNoContent,
						recorder.Body.String(),
					)
				}
				select {
				case got := <-observed:
					if got.serial != wantSerial || got.proto != test.wantProto {
						t.Fatalf(
							"upstream client = serial %d/proto %q, want %d/%q",
							got.serial,
							got.proto,
							wantSerial,
							test.wantProto,
						)
					}
				case <-time.After(time.Second):
					t.Fatal("upstream did not receive rotated mTLS request")
				}
				return builder
			}

			first := buildAndRequest(3)
			t.Cleanup(first.Stop)
			if got := registry.Len(); got != 1 {
				t.Fatalf("registry.Len() after certificate A = %d, want 1", got)
			}
			putClientCertificate(clientBCert, clientBKey)
			second := buildAndRequest(4)
			t.Cleanup(second.Stop)
			if got := registry.Len(); got != 2 {
				t.Fatalf("registry.Len() after same-ID rotation = %d, want 2", got)
			}
			third := buildAndRequest(4)
			t.Cleanup(third.Stop)
			if got := registry.Len(); got != 2 {
				t.Fatalf("registry.Len() after identical rebuild = %d, want shared 2", got)
			}

			second.Stop()
			if got := registry.Len(); got != 2 {
				t.Fatalf("registry.Len() after one rotated lease stopped = %d, want 2", got)
			}
			first.Stop()
			if got := registry.Len(); got != 1 {
				t.Fatalf("registry.Len() after old generation stopped = %d, want 1", got)
			}
			third.Stop()
			if got := registry.Len(); got != 0 {
				t.Fatalf("registry.Len() after final lease stopped = %d, want 0", got)
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
