package ldap_auth

import "testing"

func TestParityLDAPTLSVerifyDefaultsToAPISIX317(t *testing.T) {
	p := &Plugin{config: Config{
		BaseDN:  "dc=example,dc=org",
		LDAPURI: "ldap://127.0.0.1:389",
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	if p.config.TLSVerify == nil || *p.config.TLSVerify {
		t.Fatalf("tls_verify default = %v, want false as APISIX 3.17", p.config.TLSVerify)
	}
}
