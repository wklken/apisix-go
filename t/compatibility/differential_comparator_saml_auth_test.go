package pluginintegration

import (
	"bytes"
	"compress/flate"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestDifferentialSAMLAuthCasesCoverPinnedAPISIX317AuthenticationInitiation(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialCasesForPlugin("saml-auth")
	if len(cases) != 1 {
		t.Fatalf("saml-auth cases = %d, want one", len(cases))
	}
	spec := cases[0]
	if spec.Name != "saml-auth-signed-login-initiation" || spec.Plugin != "saml-auth" ||
		spec.RouteID != "differential-saml-auth-login" ||
		spec.ComparisonPolicy != "saml-auth-initiation" {
		t.Fatalf("case identity = %q/%q/%q/%q", spec.Name, spec.Plugin, spec.RouteID, spec.ComparisonPolicy)
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/probe" ||
		spec.Request.Host != "gateway.example.test" || spec.SecurityDecision != "deny" {
		t.Fatalf("request/decision = %#v/%q", spec.Request, spec.SecurityDecision)
	}
	if spec.Fixture.ExpectedCalls != 0 {
		t.Fatalf("fixture expected calls = %d, want zero", spec.Fixture.ExpectedCalls)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v", spec.Config["routes"])
	}
	route := routes[0].(map[string]any)
	plugin := route["plugins"].(map[string]any)["saml-auth"].(map[string]any)
	if plugin["sp_issuer"] != "integration-sp" ||
		plugin["idp_uri"] != "https://idp.example.test/sso" ||
		plugin["idp_entity_id"] != "https://idp.example.test/realms/integration" ||
		plugin["login_callback_uri"] != "/login/callback" ||
		plugin["auth_protocol_binding_method"] != "HTTP-Redirect" ||
		plugin["secret"] != "integration-secret" {
		t.Fatalf("saml-auth config = %#v", plugin)
	}
	for _, field := range []string{"idp_cert", "sp_cert", "sp_private_key"} {
		if value, ok := plugin[field].(string); !ok || value == "" {
			t.Fatalf("saml-auth %s = %#v, want non-empty PEM", field, plugin[field])
		}
	}
}

func TestDifferentialSAMLAuthInitiationPolicyValidatesSideLocalState(t *testing.T) {
	spec := differentialCasesForPlugin("saml-auth")[0]
	candidate := differentialSAMLAuthInitiationObservation(t, "_candidate-request", "candidate-state", "integration-sp")
	oracle := differentialSAMLAuthInitiationObservation(t, "_oracle-request", "oracle-state", "integration-sp")
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, detail, err := compareDifferentialCaseObservations(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || detail != "" {
		t.Fatalf("compare signed SAML initiations = %t, %q, %v", passed, detail, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("SAML initiation comparison mutated caller observations")
	}
}

func TestDifferentialSAMLAuthInitiationPolicyRejectsMalformedSemantics(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*DifferentialObservation, *DifferentialObservation)
		want   string
	}{
		{
			name: "missing RelayState",
			mutate: func(candidate, _ *DifferentialObservation) {
				location, err := url.Parse(candidate.Headers["Location"][0])
				if err != nil {
					t.Fatal(err)
				}
				query := location.Query()
				query.Del("RelayState")
				location.RawQuery = query.Encode()
				candidate.Headers["Location"] = []string{location.String()}
			},
			want: "RelayState",
		},
		{
			name: "unsigned request",
			mutate: func(_, oracle *DifferentialObservation) {
				location, err := url.Parse(oracle.Headers["Location"][0])
				if err != nil {
					t.Fatal(err)
				}
				query := location.Query()
				query.Set("Signature", base64.StdEncoding.EncodeToString([]byte("invalid")))
				location.RawQuery = query.Encode()
				oracle.Headers["Location"] = []string{location.String()}
			},
			want: "signature",
		},
		{
			name: "wrong issuer",
			mutate: func(candidate, _ *DifferentialObservation) {
				*candidate = differentialSAMLAuthInitiationObservation(
					t, "_candidate-request", "candidate-state", "other-sp",
				)
			},
			want: "issuer",
		},
		{
			name: "missing state cookie",
			mutate: func(_, oracle *DifferentialObservation) {
				delete(oracle.Headers, "Set-Cookie")
			},
			want: "state cookie",
		},
		{
			name: "unexpected upstream",
			mutate: func(candidate, _ *DifferentialObservation) {
				candidate.Upstream.Received = true
			},
			want: "upstream",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := differentialCasesForPlugin("saml-auth")[0]
			candidate := differentialSAMLAuthInitiationObservation(
				t, "_candidate-request", "candidate-state", "integration-sp",
			)
			oracle := differentialSAMLAuthInitiationObservation(
				t, "_oracle-request", "oracle-state", "integration-sp",
			)
			test.mutate(&candidate, &oracle)

			passed, _, err := compareDifferentialCaseObservations(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare malformed SAML initiation = %t, %v, want %q", passed, err, test.want)
			}
		})
	}
}

func differentialSAMLAuthInitiationObservation(
	t *testing.T,
	requestID string,
	relayState string,
	issuer string,
) DifferentialObservation {
	t.Helper()
	xml := `<samlp:AuthnRequest xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" ` +
		`xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="` + requestID + `" ` +
		`Version="2.0" IssueInstant="2026-08-29T01:02:03Z" ` +
		`Destination="https://idp.example.test/sso" ` +
		`AssertionConsumerServiceURL="http://gateway.example.test/login/callback" ` +
		`ProtocolBinding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect">` +
		`<saml:Issuer>` + issuer + `</saml:Issuer></samlp:AuthnRequest>`

	var compressed bytes.Buffer
	w, err := flate.NewWriter(&compressed, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(xml)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	const sigAlg = "http://www.w3.org/2001/04/xmldsig-more#rsa-sha512"
	rawQuery := "SAMLRequest=" + url.QueryEscape(base64.StdEncoding.EncodeToString(compressed.Bytes())) +
		"&RelayState=" + url.QueryEscape(relayState) +
		"&SigAlg=" + url.QueryEscape(sigAlg)
	digest := sha512.Sum512([]byte(rawQuery))
	block, _ := pem.Decode([]byte(differentialSAMLAuthPrivateKeyPEM))
	if block == nil {
		t.Fatal("decode SAML test private key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := rsa.SignPKCS1v15(rand.Reader, key.(*rsa.PrivateKey), crypto.SHA512, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	location := "https://idp.example.test/sso?" + rawQuery +
		"&Signature=" + url.QueryEscape(base64.StdEncoding.EncodeToString(signature))

	return DifferentialObservation{
		Status: http.StatusFound,
		Headers: map[string][]string{
			"Location": {location},
			"Set-Cookie": {
				"SAML_REQUEST_test_" + relayState + "=opaque; Path=/; Max-Age=600; HttpOnly; SameSite=Lax",
			},
			"Content-Type": {"text/html; charset=utf-8"},
		},
		Body:             "side-owned redirect body",
		Host:             "gateway.example.test",
		SecurityDecision: "deny",
	}
}
