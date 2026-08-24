package route

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixjson "github.com/wklken/apisix-go/pkg/json"
)

const (
	casdoorCurrentReference  = "$ENV://CAS_CURRENT"
	casdoorFallbackReference = "$ENV://CAS_FALLBACK"
	casdoorResolvedCurrent   = "route-current-route-current-route-current"
	casdoorResolvedFallback  = "route-fallback-route-fallback-route-fallback"
)

// TestBuilderDefersCasdoorSecretLengthValidationUntilMaterialization catches
// schema validation rejecting a short reference before its resolved value can
// be admitted by the plugin's secret owner.
func TestBuilderDefersCasdoorSecretLengthValidationUntilMaterialization(t *testing.T) {
	ensureRouteStore(t)
	t.Setenv("CAS_CURRENT", casdoorResolvedCurrent)
	t.Setenv("CAS_FALLBACK", casdoorResolvedFallback)
	putCasdoorSecretRoute(
		t,
		"casdoor-short-references",
		casdoorCurrentReference,
		[]string{casdoorFallbackReference},
	)

	builder := NewBuilder(nil, httpPluginAllowlist("authz-casdoor"), testDataEncryptionResolver())
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err != nil || handler == nil {
		t.Fatalf("BuildStrict() = (%T, %v), want short raw references resolved before length validation", handler, err)
	}

	request := httptest.NewRequest(http.MethodGet, "/casdoor-short-references", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusFound {
		t.Fatalf("built Casdoor route status = %d, want authorization redirect", response.Code)
	}
	if response.Header().Get("Set-Cookie") == "" {
		t.Fatal("built Casdoor route did not seal a session cookie with the resolved secret")
	}
}

func TestBuilderCasdoorResolvedLengthFailureIsRedactedAtomicAndRetryable(t *testing.T) {
	ensureRouteStore(t)
	tests := []struct {
		name           string
		current        string
		fallbacks      []string
		shortEnv       string
		shortPlaintext string
	}{
		{
			name:           "current",
			current:        casdoorCurrentReference,
			fallbacks:      []string{casdoorFallbackReference},
			shortEnv:       "CAS_CURRENT",
			shortPlaintext: "short-current-private",
		},
		{
			name:           "fallback",
			current:        casdoorCurrentReference,
			fallbacks:      []string{casdoorFallbackReference},
			shortEnv:       "CAS_FALLBACK",
			shortPlaintext: "short-fallback-private",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CAS_CURRENT", casdoorResolvedCurrent)
			t.Setenv("CAS_FALLBACK", casdoorResolvedFallback)
			t.Setenv(test.shortEnv, test.shortPlaintext)
			routeID := "casdoor-resolved-short-" + test.name
			putCasdoorSecretRoute(t, routeID, test.current, test.fallbacks)
			builder := NewBuilder(nil, httpPluginAllowlist("authz-casdoor"), testDataEncryptionResolver())
			t.Cleanup(builder.Stop)

			handler, err := builder.BuildStrict()
			if err == nil || handler != nil {
				t.Fatalf("BuildStrict() = (%T, %v), want resolved-short failure", handler, err)
			}
			for _, forbidden := range []string{
				test.current,
				test.shortPlaintext,
				"CAS_CURRENT",
				"CAS_FALLBACK",
			} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("BuildStrict() error leaked %q: %v", forbidden, err)
				}
			}
			if !strings.Contains(err.Error(), "credential unavailable") {
				t.Fatalf("BuildStrict() error = %q, want fixed credential-unavailable boundary", err)
			}
			if len(builder.stoppers) != 0 {
				t.Fatalf("failed build installed %d plugin stoppers, want zero", len(builder.stoppers))
			}

			t.Setenv(test.shortEnv, "retry-secret-retry-secret-retry-secret")
			handler, err = builder.BuildStrict()
			if err != nil || handler == nil {
				t.Fatalf("same-builder retry BuildStrict() = (%T, %v), want success", handler, err)
			}
		})
	}
}

func TestBuilderCasdoorRejectsShortLiteralAfterMaterialization(t *testing.T) {
	ensureRouteStore(t)
	const literal = "short-literal-private"
	putCasdoorSecretRoute(t, "casdoor-short-literal", literal, nil)
	builder := NewBuilder(nil, httpPluginAllowlist("authz-casdoor"), testDataEncryptionResolver())
	t.Cleanup(builder.Stop)

	handler, err := builder.BuildStrict()
	if err == nil || handler != nil {
		t.Fatalf("BuildStrict() = (%T, %v), want short literal rejection", handler, err)
	}
	if strings.Contains(err.Error(), literal) || !strings.Contains(err.Error(), "credential unavailable") {
		t.Fatalf("BuildStrict() error = %q, want redacted materialization failure", err)
	}
	if len(builder.stoppers) != 0 {
		t.Fatalf("failed literal build installed %d plugin stoppers, want zero", len(builder.stoppers))
	}
}

func putCasdoorSecretRoute(t *testing.T, id, current string, fallbacks []string) {
	t.Helper()
	config := map[string]any{
		"endpoint_addr": "https://door.example.com",
		"client_id":     "builder-client",
		"client_secret": current,
		"callback_url":  "https://gateway.example.com/callback",
	}
	if fallbacks != nil {
		config["client_secret_fallbacks"] = fallbacks
	}
	document, err := apisixjson.Marshal(map[string]any{
		"id":      id,
		"uri":     "/" + id,
		"plugins": map[string]any{"authz-casdoor": config},
	})
	if err != nil {
		t.Fatalf("marshal %s route: %v", id, err)
	}
	putRouteResource(t, id, document)
}
