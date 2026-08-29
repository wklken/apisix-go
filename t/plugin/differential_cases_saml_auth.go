package pluginintegration

import (
	"bytes"
	"compress/flate"
	"crypto"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const differentialSAMLAuthInitiationPolicy = "saml-auth-initiation"

func init() {
	differentialComparatorRegistry[differentialSAMLAuthInitiationPolicy] = differentialComparatorRegistration{
		allowedPlugins: map[string]struct{}{"saml-auth": {}},
		compare:        compareDifferentialSAMLAuthInitiation,
	}
}

const differentialSAMLAuthCertificatePEM = `-----BEGIN CERTIFICATE-----
MIICIDCCAYmgAwIBAgIUYCXLeJPz69bdMDcfhEklovNXoHQwDQYJKoZIhvcNAQEL
BQAwIjEgMB4GA1UEAwwXaW50ZWdyYXRpb24uZXhhbXBsZS5jb20wHhcNMjYwNzE1
MDM1MzU4WhcNMzYwNzEyMDM1MzU4WjAiMSAwHgYDVQQDDBdpbnRlZ3JhdGlvbi5l
eGFtcGxlLmNvbTCBnzANBgkqhkiG9w0BAQEFAAOBjQAwgYkCgYEAp6XDmwyYoSKr
y19mRcVywDB9gBK202bWQMNEF/YuUzoX4Cqi+AJu2AMIbYJHp3plJzZUjYrAapWb
PTjFMPkbzKc+nSdqe28bzAEhs80UnFXGpMB009w8c3uJ1DZjzCJ+Pp4OWOk/IgOF
3eRuUAG9ztmo/0T0K/89bJ9U9bkixlMCAwEAAaNTMFEwHQYDVR0OBBYEFGdllZXf
2uPPyvGM4lfumvybA6NSMB8GA1UdIwQYMBaAFGdllZXf2uPPyvGM4lfumvybA6NS
MA8GA1UdEwEB/wQFMAMBAf8wDQYJKoZIhvcNAQELBQADgYEAcxEi38GG2eqfl2Ps
2D+k56X08tgMdXLFKNI9AI+SIPogsDFjVRbNKEA5a2iWEI78Q5qz1brqQ3KWKjHE
gWuujK4lQkYhGY2342j6jiqVjv8YVBMImBKoo0sGJj5ACY4a0jho8t47PBIwzc7U
D+khHyKxVHAgTMNrLOgK7+FyEGQ=
-----END CERTIFICATE-----`

const differentialSAMLAuthPrivateKeyPEM = `-----BEGIN PRIVATE KEY-----
MIICdQIBADANBgkqhkiG9w0BAQEFAASCAl8wggJbAgEAAoGBAKelw5sMmKEiq8tf
ZkXFcsAwfYASttNm1kDDRBf2LlM6F+AqovgCbtgDCG2CR6d6ZSc2VI2KwGqVmz04
xTD5G8ynPp0nantvG8wBIbPNFJxVxqTAdNPcPHN7idQ2Y8wifj6eDljpPyIDhd3k
blABvc7ZqP9E9Cv/PWyfVPW5IsZTAgMBAAECgYAu2SnCSFDWpqOvX2drE/QvNN29
Tn18sf4pdueucoMbit5lLEUCXVuwTZirUX7IlHFz9cDHFQEUR95ry1N/jf1wTXF0
q9dMK1sOxeIxD5u87IT/YUFOGR37PpfzBqr4lXBqSFHOr3pnMyboyYzl/hsR5QkC
ytxBICCR17uQAU1z0QJBANSScivNYrzQA+jLrnUUMerY3VJUy7OelC7aOvM+w0Wg
g2y7wLWcG0vQpJEeLh6xXPxu/98Q57gDkUjEu4G8c30CQQDJ5cHY63WIHr3SX+6s
cqO5tklJKB7TYBxZXJKLRewW6xkzcjMXlLsZzn9oFtr5e6ggcyvvViQmjH9UBxNC
s6oPAkApUp6nLTH4imd4JcAwOlDJ2oaLrrg6nqUnxnyXNKg5LM7foFAB/erAfjq/
iyJkDQ6Kc/mBn4OsHeVsQ/I/cibxAkBfHMf3guU5nRHbu6nav5717DQWLLpo5cw1
JPE8f1I7ccHLhK8hGsYR4EARL0M1aNXJg7hc5f3d0y5gzXx7XdxtAkATXODoxB8h
KbxxbtNMlYkGCrYKfZgiZWstfYYAOliciGSqmHE2eqED/424yuTO9m4PIyKXKWwS
JKuSmg0k5oxG
-----END PRIVATE KEY-----`

// differentialSAMLAuthCases maps the authentication-initiation portion of
// APISIX 3.17 t/plugin/saml-auth.t TEST 11. The redirect, RelayState, signed
// AuthnRequest and state cookie are validated per side because their values
// are deliberately random and therefore must not be compared byte-for-byte.
func differentialSAMLAuthCases() []DifferentialCase {
	const routeID = "differential-saml-auth-login"

	return []DifferentialCase{{
		Name:             "saml-auth-signed-login-initiation",
		Plugin:           "saml-auth",
		RouteID:          routeID,
		ComparisonPolicy: differentialSAMLAuthInitiationPolicy,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/probe", "host": "gateway.example.test",
				"plugins": map[string]any{"saml-auth": map[string]any{
					"sp_issuer":                    "integration-sp",
					"idp_uri":                      "https://idp.example.test/sso",
					"idp_entity_id":                "https://idp.example.test/realms/integration",
					"idp_cert":                     differentialSAMLAuthCertificatePEM,
					"login_callback_uri":           "/login/callback",
					"logout_uri":                   "/logout",
					"logout_callback_uri":          "/logout/callback",
					"logout_redirect_uri":          "/logged-out",
					"sp_cert":                      differentialSAMLAuthCertificatePEM,
					"sp_private_key":               differentialSAMLAuthPrivateKeyPEM,
					"auth_protocol_binding_method": "HTTP-Redirect",
					"secret":                       "integration-secret",
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/probe", Host: "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unexpected"},
		},
		SecurityDecision: "deny",
	}}
}

type differentialSAMLAuthnRequest struct {
	XMLName                     xml.Name
	ID                          string `xml:"ID,attr"`
	Version                     string `xml:"Version,attr"`
	IssueInstant                string `xml:"IssueInstant,attr"`
	Destination                 string `xml:"Destination,attr"`
	AssertionConsumerServiceURL string `xml:"AssertionConsumerServiceURL,attr"`
	ProtocolBinding             string `xml:"ProtocolBinding,attr"`
	Issuer                      string `xml:"Issuer"`
}

func compareDifferentialSAMLAuthInitiation(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if spec.Name != "saml-auth-signed-login-initiation" ||
		spec.RouteID != "differential-saml-auth-login" ||
		spec.Request.Method != http.MethodGet || spec.Request.Path != "/probe" ||
		spec.Request.Host != "gateway.example.test" || spec.Request.Body != "" ||
		spec.Fixture.ExpectedCalls != 0 || spec.SecurityDecision != "deny" || len(spec.Steps) != 0 {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the pinned SAML authentication-initiation case",
			spec.ComparisonPolicy,
		)
	}

	left = copyDifferentialObservation(left)
	right = copyDifferentialObservation(right)
	for _, side := range []struct {
		name        string
		observation *DifferentialObservation
	}{
		{name: "candidate", observation: &left},
		{name: "oracle", observation: &right},
	} {
		if err := validateDifferentialSAMLAuthInitiationSide(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}

	return compareNormalizedObservations(left, right, policy)
}

func validateDifferentialSAMLAuthInitiationSide(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if observation.Status != http.StatusFound || observation.SecurityDecision != "deny" ||
		observation.Host != spec.Request.Host || observation.SNI != "" || len(observation.Steps) != 0 {
		return fmt.Errorf(
			"comparison policy %q requires %s 302 deny for the pinned Host",
			spec.ComparisonPolicy,
			side,
		)
	}
	if err := validateDifferentialSAMLAuthNoUpstream(spec, side, *observation); err != nil {
		return err
	}

	location, err := singleDifferentialHeader(observation.Headers, "Location")
	if err != nil {
		return fmt.Errorf("comparison policy %q %s Location: %w", spec.ComparisonPolicy, side, err)
	}
	parsed, err := url.Parse(location)
	if err != nil {
		return fmt.Errorf("comparison policy %q parse %s Location: %w", spec.ComparisonPolicy, side, err)
	}
	if parsed.Scheme != "https" || parsed.Host != "idp.example.test" || parsed.Path != "/sso" ||
		parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf(
			"comparison policy %q %s Location is not the configured IdP endpoint",
			spec.ComparisonPolicy,
			side,
		)
	}
	query := parsed.Query()
	for _, name := range []string{"SAMLRequest", "RelayState", "SigAlg", "Signature"} {
		values := query[name]
		if len(values) != 1 || values[0] == "" {
			return fmt.Errorf(
				"comparison policy %q %s %s must have exactly one non-empty value",
				spec.ComparisonPolicy,
				side,
				name,
			)
		}
	}
	if len(query) != 4 {
		return fmt.Errorf(
			"comparison policy %q %s SAML redirect has unexpected query keys",
			spec.ComparisonPolicy,
			side,
		)
	}
	if query.Get("SigAlg") != "http://www.w3.org/2001/04/xmldsig-more#rsa-sha512" {
		return fmt.Errorf("comparison policy %q %s SigAlg is not RSA-SHA512", spec.ComparisonPolicy, side)
	}
	if err := verifyDifferentialSAMLAuthRedirectSignature(parsed.RawQuery); err != nil {
		return fmt.Errorf("comparison policy %q %s signature: %w", spec.ComparisonPolicy, side, err)
	}
	if err := validateDifferentialSAMLAuthRequest(query.Get("SAMLRequest")); err != nil {
		return fmt.Errorf("comparison policy %q %s AuthnRequest: %w", spec.ComparisonPolicy, side, err)
	}

	cookieValues := differentialHeaderValues(observation.Headers, "Set-Cookie")
	if len(cookieValues) != 1 {
		return fmt.Errorf(
			"comparison policy %q requires one %s state cookie, got %d",
			spec.ComparisonPolicy,
			side,
			len(cookieValues),
		)
	}
	cookie, err := http.ParseSetCookie(cookieValues[0])
	if err != nil || cookie.Name == "" || cookie.Value == "" || cookie.Path != "/" {
		return fmt.Errorf(
			"comparison policy %q %s state cookie is malformed: %v",
			spec.ComparisonPolicy,
			side,
			err,
		)
	}

	// The request ID, IssueInstant, RelayState, signature, cookie name/value,
	// and response body are generated independently on each side. Validate
	// them above, then replace only those proven-dynamic representations.
	deleteDifferentialHeader(observation.Headers, "Location")
	deleteDifferentialHeader(observation.Headers, "Set-Cookie")
	deleteDifferentialHeader(observation.Headers, "Content-Type")
	deleteDifferentialHeader(observation.Headers, "Content-Length")
	observation.Headers["Location"] = []string{"saml-auth:validated-idp-redirect"}
	observation.Headers["Set-Cookie"] = []string{"saml-auth:validated-state-cookie"}
	observation.Body = ""
	return nil
}

func validateDifferentialSAMLAuthRequest(encoded string) error {
	compressed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode base64: %w", err)
	}
	reader := flate.NewReader(bytes.NewReader(compressed))
	defer func() { _ = reader.Close() }()
	raw, err := io.ReadAll(io.LimitReader(reader, 1<<20))
	if err != nil {
		return fmt.Errorf("inflate: %w", err)
	}
	var request differentialSAMLAuthnRequest
	if err := xml.Unmarshal(raw, &request); err != nil {
		return fmt.Errorf("decode XML: %w", err)
	}
	if request.XMLName.Space != "urn:oasis:names:tc:SAML:2.0:protocol" ||
		request.XMLName.Local != "AuthnRequest" || request.ID == "" || request.Version != "2.0" {
		return fmt.Errorf("invalid AuthnRequest identity")
	}
	if _, err := time.Parse(time.RFC3339, request.IssueInstant); err != nil {
		return fmt.Errorf("invalid IssueInstant: %w", err)
	}
	if request.Destination != "https://idp.example.test/sso" ||
		request.AssertionConsumerServiceURL != "http://gateway.example.test/login/callback" ||
		request.ProtocolBinding != "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" {
		return fmt.Errorf("invalid Destination, callback, or ProtocolBinding")
	}
	if strings.TrimSpace(request.Issuer) != "integration-sp" {
		return fmt.Errorf(
			"issuer = %q, want integration-sp",
			strings.TrimSpace(request.Issuer),
		)
	}
	return nil
}

func verifyDifferentialSAMLAuthRedirectSignature(rawQuery string) error {
	const marker = "&Signature="
	index := strings.LastIndex(rawQuery, marker)
	if index <= 0 || strings.Contains(rawQuery[index+len(marker):], "&") {
		return fmt.Errorf(
			"signature must be the final query field",
		)
	}
	signatureValue, err := url.QueryUnescape(rawQuery[index+len(marker):])
	if err != nil {
		return fmt.Errorf("unescape: %w", err)
	}
	signature, err := base64.StdEncoding.DecodeString(signatureValue)
	if err != nil {
		return fmt.Errorf("decode base64: %w", err)
	}
	block, _ := pem.Decode([]byte(differentialSAMLAuthCertificatePEM))
	if block == nil {
		return fmt.Errorf("decode SP certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse SP certificate: %w", err)
	}
	publicKey, ok := certificate.PublicKey.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("SP certificate does not contain an RSA key")
	}
	digest := sha512.Sum512([]byte(rawQuery[:index]))
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA512, digest[:], signature); err != nil {
		return err
	}
	return nil
}

func validateDifferentialSAMLAuthNoUpstream(
	spec DifferentialCase,
	side string,
	observation DifferentialObservation,
) error {
	upstream := observation.Upstream
	if upstream.Received || upstream.Fixture != "" || upstream.Method != "" || upstream.Path != "" ||
		upstream.Host != "" || len(upstream.Headers) != 0 || upstream.Body != "" ||
		observation.UpstreamFixture != "" || observation.UpstreamAddress != "" ||
		len(observation.UpstreamCalls) != 0 || observation.RetryCount != 0 {
		return fmt.Errorf(
			"comparison policy %q requires no %s upstream activity",
			spec.ComparisonPolicy,
			side,
		)
	}
	return nil
}
