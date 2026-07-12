package kafka_proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go/sasl/plain"
)

func TestNewKafkaConsumerConfiguresTLSAndSASLPlain(t *testing.T) {
	tlsConfig := &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	consumer, err := newKafkaConsumer(context.Background(), []string{"kafka://broker.test:9093"}, ConsumerOptions{
		TLSConfig:    tlsConfig,
		SASLEnabled:  true,
		SASLUsername: "user",
		SASLPassword: "password",
	})
	if err != nil {
		t.Fatalf("newKafkaConsumer() error = %v", err)
	}
	configured, ok := consumer.(*kafkaConsumer)
	if !ok {
		t.Fatalf("consumer type = %T, want *kafkaConsumer", consumer)
	}
	if configured.dialer.TLS != tlsConfig {
		t.Fatalf("dialer.TLS = %#v, want supplied TLS config", configured.dialer.TLS)
	}
	mechanism, ok := configured.dialer.SASLMechanism.(plain.Mechanism)
	if !ok {
		t.Fatalf("SASL mechanism = %T, want plain.Mechanism", configured.dialer.SASLMechanism)
	}
	if mechanism.Username != "user" || mechanism.Password != "password" {
		t.Fatalf("SASL mechanism = %#v, want configured credentials", mechanism)
	}
}

func TestKafkaConsumerRejectsInvalidRequestBeforeDialing(t *testing.T) {
	consumer, err := newKafkaConsumer(context.Background(), []string{"kafka://127.0.0.1:1"}, ConsumerOptions{})
	if err != nil {
		t.Fatalf("newKafkaConsumer() error = %v", err)
	}
	if _, err := consumer.Fetch(context.Background(), "", 0, 0); err == nil {
		t.Fatal("Fetch() error = nil, want empty topic rejection")
	}
	if _, err := consumer.ListOffset(context.Background(), "topic", -1, -2); err == nil {
		t.Fatal("ListOffset() error = nil, want negative partition rejection")
	}
}

func TestKafkaConsumerTLSAndSASLFixtureMapsBrokerAuthError(t *testing.T) {
	serverCert, roots := kafkaFixtureCertificate(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls.Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	authPayload := make(chan []byte, 1)
	fixtureErr := make(chan error, 1)
	go serveKafkaTLSFixture(listener, authPayload, fixtureErr)

	consumer, err := newKafkaConsumer(context.Background(), []string{
		"kafka://" + listener.Addr().String(),
	}, ConsumerOptions{
		ConnectTimeout: time.Second,
		ReadTimeout:    time.Second,
		TLSConfig:      &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		SASLEnabled:    true,
		SASLUsername:   "fixture-user",
		SASLPassword:   "fixture-password",
	})
	if err != nil {
		t.Fatalf("newKafkaConsumer() error = %v", err)
	}
	_, err = consumer.ListOffset(context.Background(), "fixture-topic", 0, -2)
	if err == nil || !strings.Contains(err.Error(), "SASL Authentication failed") {
		select {
		case fixtureErr := <-fixtureErr:
			t.Fatalf("ListOffset() error = %v; fixture error = %v", err, fixtureErr)
		default:
		}
		t.Fatalf("ListOffset() error = %v, want broker SASL authentication error", err)
	}
	select {
	case payload := <-authPayload:
		if want := []byte("\x00fixture-user\x00fixture-password"); !bytes.Equal(payload, want) {
			t.Fatalf("SASL PLAIN payload = %q, want %q", payload, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SASL payload")
	}
	select {
	case err := <-fixtureErr:
		t.Fatalf("Kafka TLS fixture error = %v", err)
	default:
	}
}

func serveKafkaTLSFixture(listener net.Listener, authPayload chan<- []byte, fixtureErr chan<- error) {
	conn, err := listener.Accept()
	if err != nil {
		fixtureErr <- err
		return
	}
	defer conn.Close()
	if err := serveKafkaFixtureConnection(conn, authPayload); err != nil {
		fixtureErr <- err
	}
}

func serveKafkaFixtureConnection(conn net.Conn, authPayload chan<- []byte) error {
	reader := bufio.NewReader(conn)
	for {
		frame, err := readKafkaFixtureFrame(reader)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if len(frame) < 8 {
			return fmt.Errorf("Kafka fixture request is too short: %d", len(frame))
		}
		apiKey := binary.BigEndian.Uint16(frame[0:2])
		correlation := binary.BigEndian.Uint32(frame[4:8])
		switch apiKey {
		case 18:
			if err := writeKafkaFixtureFrame(conn, correlation, kafkaFixtureApiVersionsResponse()); err != nil {
				return err
			}
		case 17:
			if err := writeKafkaFixtureFrame(conn, correlation, kafkaFixtureSASLHandshakeResponse()); err != nil {
				return err
			}
		case 36:
			payload, err := kafkaFixtureSASLPayload(frame)
			if err != nil {
				return err
			}
			authPayload <- payload
			if err := writeKafkaFixtureFrame(conn, correlation, kafkaFixtureSASLAuthResponse()); err != nil {
				return err
			}
			return nil
		default:
			return fmt.Errorf("Kafka fixture received unexpected API key %d", apiKey)
		}
	}
}

func readKafkaFixtureFrame(reader *bufio.Reader) ([]byte, error) {
	var size int32
	if err := binary.Read(reader, binary.BigEndian, &size); err != nil {
		return nil, err
	}
	if size < 0 || size > 1<<20 {
		return nil, fmt.Errorf("Kafka fixture frame size %d is invalid", size)
	}
	frame := make([]byte, size)
	_, err := io.ReadFull(reader, frame)
	return frame, err
}

func writeKafkaFixtureFrame(conn net.Conn, correlation uint32, body []byte) error {
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame[0:4], correlation)
	copy(frame[4:], body)
	if err := binary.Write(conn, binary.BigEndian, int32(len(frame))); err != nil {
		return err
	}
	_, err := conn.Write(frame)
	return err
}

func kafkaFixtureApiVersionsResponse() []byte {
	body := make([]byte, 2+4+3*2)
	binary.BigEndian.PutUint16(body[0:2], 0)
	binary.BigEndian.PutUint32(body[2:6], 1)
	putKafkaFixtureAPIKey(body[6:], 17, 0, 1)
	return body
}

func putKafkaFixtureAPIKey(dst []byte, key, minVersion, maxVersion uint16) {
	binary.BigEndian.PutUint16(dst[0:2], key)
	binary.BigEndian.PutUint16(dst[2:4], minVersion)
	binary.BigEndian.PutUint16(dst[4:6], maxVersion)
}

func kafkaFixtureSASLHandshakeResponse() []byte {
	return []byte{0, 0, 0, 0, 0, 1, 0, 5, 'P', 'L', 'A', 'I', 'N'}
}

func kafkaFixtureSASLPayload(frame []byte) ([]byte, error) {
	if len(frame) < 10 {
		return nil, fmt.Errorf("Kafka fixture SASL request is too short")
	}
	clientIDLength := int(int16(binary.BigEndian.Uint16(frame[8:10])))
	if clientIDLength < 0 || len(frame) < 10+clientIDLength+4 {
		return nil, fmt.Errorf("Kafka fixture SASL request has invalid client id")
	}
	dataLengthOffset := 10 + clientIDLength
	dataLength := int32(binary.BigEndian.Uint32(frame[dataLengthOffset : dataLengthOffset+4]))
	if dataLength < 0 || len(frame) < dataLengthOffset+4+int(dataLength) {
		return nil, fmt.Errorf("Kafka fixture SASL request has invalid payload")
	}
	return append([]byte(nil), frame[dataLengthOffset+4:dataLengthOffset+4+int(dataLength)]...), nil
}

func kafkaFixtureSASLAuthResponse() []byte {
	message := "invalid credentials"
	body := make([]byte, 2+2+len(message)+4)
	binary.BigEndian.PutUint16(body[0:2], 58)
	binary.BigEndian.PutUint16(body[2:4], uint16(len(message)))
	copy(body[4:4+len(message)], message)
	binary.BigEndian.PutUint32(body[4+len(message):], 0xffffffff)
	return body
}

func kafkaFixtureCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		t.Fatalf("rand.Int() error = %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate() error = %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair() error = %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("AppendCertsFromPEM() rejected fixture certificate")
	}
	return cert, roots
}
