package ldap_auth

import (
	"errors"
	"net"
	"testing"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
	"github.com/go-ldap/ldap/v3"
)

type ldapBindObservation struct {
	dn       string
	password string
	closed   bool
	err      error
}

func startLDAPBindFixture(t *testing.T, resultCode uint16) (string, <-chan ldapBindObservation) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen LDAP fixture: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	observed := make(chan ldapBindObservation, 1)
	go func() {
		observation := ldapBindObservation{}
		defer func() { observed <- observation }()
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			observation.err = acceptErr
			return
		}
		defer func() { _ = connection.Close() }()
		request, readErr := ber.ReadPacket(connection)
		if readErr != nil {
			observation.err = readErr
			return
		}
		if len(request.Children) != 2 || len(request.Children[1].Children) != 3 {
			observation.err = errors.New("unexpected LDAP bind packet shape")
			return
		}
		bind := request.Children[1]
		observation.dn, _ = bind.Children[1].Value.(string)
		observation.password = bind.Children[2].Data.String()

		response := ber.NewSequence("LDAPMessage")
		response.AppendChild(ber.NewInteger(
			ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, request.Children[0].Value, "messageID",
		))
		bindResponse := ber.Encode(
			ber.ClassApplication, ber.TypeConstructed, ldap.ApplicationBindResponse, nil, "Bind Response",
		)
		bindResponse.AppendChild(ber.NewInteger(
			ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, int64(resultCode), "resultCode",
		))
		bindResponse.AppendChild(ber.NewString(
			ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "matchedDN",
		))
		bindResponse.AppendChild(ber.NewString(
			ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "fixture", "diagnosticMessage",
		))
		response.AppendChild(bindResponse)
		if _, writeErr := connection.Write(response.Bytes()); writeErr != nil {
			observation.err = writeErr
			return
		}
		if deadlineErr := connection.SetReadDeadline(time.Now().Add(time.Second)); deadlineErr != nil {
			observation.err = deadlineErr
			return
		}
		buffer := make([]byte, 1)
		for {
			_, readErr = connection.Read(buffer)
			if readErr != nil {
				if networkErr, ok := readErr.(net.Error); ok && networkErr.Timeout() {
					observation.err = readErr
					return
				}
				observation.closed = true
				return
			}
		}
	}()
	return listener.Addr().String(), observed
}

func TestDefaultLDAPAuthenticateUsesProtocolAndClosesConnection(t *testing.T) {
	address, observed := startLDAPBindFixture(t, ldap.LDAPResultSuccess)
	err := defaultLDAPAuthenticate("alice", "secret", Config{
		BaseDN: "dc=example,dc=com", LDAPURI: address, UID: "uid",
	})
	if err != nil {
		t.Fatalf("defaultLDAPAuthenticate() error = %v", err)
	}
	observation := <-observed
	if observation.err != nil {
		t.Fatalf("LDAP fixture error = %v", observation.err)
	}
	if observation.dn != "uid=alice,dc=example,dc=com" || observation.password != "secret" {
		t.Fatalf("LDAP bind = %q/%q, want protocol credentials", observation.dn, observation.password)
	}
	if !observation.closed {
		t.Fatal("LDAP connection was not closed after bind")
	}
}

func TestDefaultLDAPAuthenticateRejectsProviderAndConnectFailures(t *testing.T) {
	address, observed := startLDAPBindFixture(t, ldap.LDAPResultInvalidCredentials)
	if err := defaultLDAPAuthenticate("alice", "wrong", Config{
		BaseDN: "dc=example,dc=com", LDAPURI: address, UID: "uid",
	}); err == nil {
		t.Fatal("defaultLDAPAuthenticate() accepted invalid LDAP credentials")
	}
	if observation := <-observed; observation.err != nil {
		t.Fatalf("LDAP rejection fixture error = %v", observation.err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve closed LDAP address: %v", err)
	}
	closedAddress := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close reserved LDAP listener: %v", err)
	}
	if err := defaultLDAPAuthenticate("alice", "secret", Config{
		BaseDN: "dc=example,dc=com", LDAPURI: closedAddress, UID: "uid",
	}); err == nil {
		t.Fatal("defaultLDAPAuthenticate() accepted LDAP connect failure")
	}
}

func TestDefaultLDAPAuthenticateRejectsMalformedProtocolResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen malformed LDAP fixture: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	fixtureDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			fixtureDone <- acceptErr
			return
		}
		defer func() { _ = connection.Close() }()
		request, readErr := ber.ReadPacket(connection)
		if readErr != nil {
			fixtureDone <- readErr
			return
		}
		response := ber.NewSequence("malformed LDAP response")
		response.AppendChild(ber.NewInteger(
			ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, request.Children[0].Value, "messageID",
		))
		_, writeErr := connection.Write(response.Bytes())
		fixtureDone <- writeErr
	}()

	if err := defaultLDAPAuthenticate("alice", "secret", Config{
		BaseDN: "dc=example,dc=com", LDAPURI: listener.Addr().String(), UID: "uid",
	}); err == nil {
		t.Fatal("defaultLDAPAuthenticate() accepted malformed LDAP response")
	}
	if err := <-fixtureDone; err != nil {
		t.Fatalf("malformed LDAP fixture error = %v", err)
	}
}

func TestDefaultLDAPAuthenticateBoundsStalledBindAtAPISIX317Timeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen stalled LDAP fixture: %v", err)
	}
	fixtureReady := make(chan struct{})
	releaseFixture := make(chan struct{})
	fixtureDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			fixtureDone <- acceptErr
			return
		}
		defer func() { _ = connection.Close() }()
		if _, readErr := ber.ReadPacket(connection); readErr != nil {
			fixtureDone <- readErr
			return
		}
		close(fixtureReady)
		<-releaseFixture
		fixtureDone <- nil
	}()

	authDone := make(chan error, 1)
	started := time.Now()
	go func() {
		authDone <- defaultLDAPAuthenticate("alice", "secret", Config{
			BaseDN: "dc=example,dc=com", LDAPURI: listener.Addr().String(), UID: "uid",
		})
	}()

	select {
	case <-fixtureReady:
	case <-time.After(time.Second):
		close(releaseFixture)
		_ = listener.Close()
		t.Fatal("LDAP client did not send Bind request")
	}

	var authErr error
	select {
	case authErr = <-authDone:
	case <-time.After(12 * time.Second):
		close(releaseFixture)
		_ = listener.Close()
		<-authDone
		<-fixtureDone
		t.Fatal("stalled LDAP Bind exceeded APISIX 3.17 10-second timeout")
	}
	close(releaseFixture)
	_ = listener.Close()
	if err := <-fixtureDone; err != nil {
		t.Fatalf("stalled LDAP fixture error = %v", err)
	}
	if authErr == nil {
		t.Fatal("stalled LDAP Bind returned nil error")
	}
	elapsed := time.Since(started)
	if elapsed < 9*time.Second || elapsed > 12*time.Second {
		t.Fatalf("stalled LDAP Bind elapsed = %s, want APISIX 3.17 10-second bound", elapsed)
	}
}
