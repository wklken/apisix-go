package saml_auth

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewjam/saml"
)

func TestAbsoluteURL(t *testing.T) {
	tests := []struct {
		name   string
		host   string
		tls    bool
		rawURL string
		want   string
	}{
		{name: "absolute preserved", host: "api.test", rawURL: "https://idp.test/sso", want: "https://idp.test/sso"},
		{name: "relative http", host: "api.test", rawURL: "/callback", want: "http://api.test/callback"},
		{name: "relative https", host: "api.test", tls: true, rawURL: "/callback", want: "https://api.test/callback"},
		{name: "missing slash", host: "api.test", rawURL: "callback", want: "http://api.test/callback"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://api.test/", nil)
			request.Host = test.host
			if test.tls {
				request.TLS = &tls.ConnectionState{}
			}
			got, err := absoluteURL(request, test.rawURL)
			if err != nil {
				t.Fatalf("absoluteURL() error = %v", err)
			}
			if got.String() != test.want {
				t.Fatalf("absoluteURL(%q) = %q, want %q", test.rawURL, got.String(), test.want)
			}
		})
	}
	if _, err := absoluteURL(httptest.NewRequest(http.MethodGet, "/", nil), "://bad"); err == nil {
		t.Fatal("absoluteURL(invalid) error = nil")
	}
}

func TestSafeRedirect(t *testing.T) {
	tests := []struct {
		uri  string
		want bool
	}{
		{uri: "/callback", want: true},
		{uri: "/callback?state=1", want: true},
		{uri: ""},
		{uri: "https://evil.test"},
		{uri: "//evil.test"},
		{uri: "/callback\\backslash"},
		{uri: "/callback\r\ninjected"},
	}
	for _, test := range tests {
		if got := safeRedirect(test.uri); got != test.want {
			t.Fatalf("safeRedirect(%q) = %t, want %t", test.uri, got, test.want)
		}
	}
}

func TestUserFromAssertion(t *testing.T) {
	assertion := &saml.Assertion{
		Subject: &saml.Subject{NameID: &saml.NameID{Value: "bob"}},
		AttributeStatements: []saml.AttributeStatement{{
			Attributes: []saml.Attribute{
				{FriendlyName: "email", Values: []saml.AttributeValue{{Value: "bob@example.test"}}},
				{Name: "groups", Values: []saml.AttributeValue{{Value: "admin"}}},
				{FriendlyName: "id", Values: []saml.AttributeValue{{NameID: &saml.NameID{Value: "id-1"}}}},
				{Values: []saml.AttributeValue{{Value: "ignored"}}},
			},
		}},
	}

	user := userFromAssertion(assertion)
	if user.NameID != "bob" {
		t.Fatalf("NameID = %q, want bob", user.NameID)
	}
	if got := user.Attributes["email"]; len(got) != 1 || got[0] != "bob@example.test" {
		t.Fatalf("email = %v", got)
	}
	if got := user.Attributes["groups"]; len(got) != 1 || got[0] != "admin" {
		t.Fatalf("groups = %v", got)
	}
	if got := user.Attributes["id"]; len(got) != 1 || got[0] != "id-1" {
		t.Fatalf("id = %v", got)
	}

	empty := userFromAssertion(&saml.Assertion{})
	if empty.NameID != "" || empty.Attributes != nil {
		t.Fatalf("empty assertion user = %+v, want no attributes", empty)
	}
}
