package pluginintegration

import (
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type ldapFixture struct {
	listener  net.Listener
	expect    []NetworkAssertion
	respond   []NetworkResponse
	received  chan []byte
	errors    chan error
	done      chan struct{}
	closeOnce sync.Once
	sequence  sync.Mutex
	next      int
	wg        sync.WaitGroup
}

func startLDAPFixture(spec FixtureSpec) (namedFixture, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen LDAP fixture: %w", err)
	}
	fixture := &ldapFixture{
		listener: listener,
		expect:   spec.NetworkExpect,
		respond:  spec.NetworkRespond,
		received: make(chan []byte, len(spec.NetworkExpect)+1),
		errors:   make(chan error, len(spec.NetworkExpect)+1),
		done:     make(chan struct{}),
	}
	fixture.wg.Add(1)
	go fixture.serve()
	return fixture, nil
}

func (f *ldapFixture) serve() {
	defer f.wg.Done()
	for {
		connection, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.done:
				return
			default:
			}
			f.errors <- fmt.Errorf("accept LDAP fixture connection: %w", err)
			return
		}
		f.wg.Go(func() {
			f.serveConnection(connection)
		})
	}
}

func (f *ldapFixture) serveConnection(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	for {
		packet, err := readLDAPPacket(connection)
		if err != nil {
			if err != io.EOF {
				f.errors <- fmt.Errorf("read LDAP packet: %w", err)
			}
			return
		}
		index := f.nextResponse()
		f.received <- packet
		response, err := networkResponseBytes(f.response(index, packet))
		if err != nil {
			f.errors <- fmt.Errorf("decode LDAP response %d: %w", index+1, err)
			return
		}
		if f.respond[index].Delay > 0 {
			time.Sleep(f.respond[index].Delay)
		}
		if _, err := connection.Write(response); err != nil {
			f.errors <- fmt.Errorf("write LDAP response %d: %w", index+1, err)
			return
		}
		if f.respond[index].Close || index == len(f.respond)-1 {
			return
		}
	}
}

func (f *ldapFixture) response(index int, packet []byte) NetworkResponse {
	if index < len(f.respond) && (f.respond[index].Payload != "" || f.respond[index].PayloadBase64 != "") {
		return f.respond[index]
	}
	messageID := ldapMessageID(packet)
	if index < len(f.respond) && index >= 0 {
		return NetworkResponse{PayloadBase64: encodeLDAPBindSuccess(messageID)}
	}
	return NetworkResponse{PayloadBase64: encodeLDAPBindSuccess(messageID)}
}

func readLDAPPacket(reader io.Reader) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	if header[0] != 0x30 {
		return nil, fmt.Errorf("LDAP packet is not a sequence")
	}
	lengthBytes, err := readLDAPLength(reader, header[1])
	if err != nil {
		return nil, err
	}
	packet := append([]byte(nil), header...)
	packet = append(packet, lengthBytes...)
	length := ldapLengthValue(header[1], lengthBytes)
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	return append(packet, body...), nil
}

func readLDAPLength(reader io.Reader, first byte) ([]byte, error) {
	if first&0x80 == 0 {
		return nil, nil
	}
	count := int(first & 0x7f)
	if count == 0 || count > 4 {
		return nil, fmt.Errorf("LDAP length encoding is invalid")
	}
	length := make([]byte, count)
	if _, err := io.ReadFull(reader, length); err != nil {
		return nil, err
	}
	return length, nil
}

func ldapLengthValue(first byte, lengthBytes []byte) int {
	if first&0x80 == 0 {
		return int(first)
	}
	value := 0
	for _, item := range lengthBytes {
		value = value<<8 | int(item)
	}
	return value
}

func ldapMessageID(packet []byte) byte {
	if len(packet) >= 5 && packet[2] == 0x02 && packet[3] == 0x01 {
		return packet[4]
	}
	return 1
}

func encodeLDAPBindSuccess(messageID byte) string {
	packet := []byte{
		0x30, 0x0c,
		0x02, 0x01, messageID,
		0x61, 0x07,
		0x0a, 0x01, 0x00,
		0x04, 0x00,
		0x04, 0x00,
	}
	return encodeBase64(packet)
}

func encodeBase64(payload []byte) string {
	return base64.StdEncoding.EncodeToString(payload)
}

func (f *ldapFixture) nextResponse() int {
	f.sequence.Lock()
	defer f.sequence.Unlock()
	index := f.next
	f.next++
	return index
}

func (f *ldapFixture) address() string { return f.listener.Addr().String() }
func (f *ldapFixture) host() string {
	host, _, _ := net.SplitHostPort(f.address())
	return host
}

func (f *ldapFixture) port() string {
	_, port, _ := net.SplitHostPort(f.address())
	return port
}
func (f *ldapFixture) url() string { return "ldap://" + f.address() }
func (f *ldapFixture) close() {
	f.closeOnce.Do(func() {
		close(f.done)
		_ = f.listener.Close()
		f.wg.Wait()
	})
}

func (f *ldapFixture) assert(t *testing.T, spec FixtureSpec) {
	t.Helper()
	for i, expected := range spec.NetworkExpect {
		select {
		case payload := <-f.received:
			if err := matchNetworkAssertion(expected, payload); err != nil {
				t.Errorf("fixture %s packet %d: %v", spec.Name, i+1, err)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("fixture %s did not receive expected packet %d", spec.Name, i+1)
		}
	}
	select {
	case err := <-f.errors:
		t.Errorf("fixture %s: %v", spec.Name, err)
	default:
	}
}

func TestLDAPFixtureReturnsBindSuccess(t *testing.T) {
	request := []byte{
		0x30, 0x0f,
		0x02, 0x01, 0x01,
		0x60, 0x0a,
		0x02, 0x01, 0x03,
		0x04, 0x00,
		0x80, 0x03, 'p', 'w', 'd',
	}
	response, err := base64.StdEncoding.DecodeString(encodeLDAPBindSuccess(1))
	if err != nil {
		t.Fatalf("decode LDAP response: %v", err)
	}
	spec := FixtureSpec{
		Name: "ldap",
		Kind: "ldap",
		NetworkExpect: []NetworkAssertion{{
			PayloadBase64: &Matcher{Equals: new(base64.StdEncoding.EncodeToString(request))},
		}},
		NetworkRespond: []NetworkResponse{{
			PayloadBase64: base64.StdEncoding.EncodeToString(response),
		}},
	}
	fixture, err := startLDAPFixture(spec)
	if err != nil {
		t.Fatalf("start LDAP fixture: %v", err)
	}
	defer fixture.close()
	connection, err := net.Dial("tcp", fixture.address())
	if err != nil {
		t.Fatalf("dial LDAP fixture: %v", err)
	}
	defer func() { _ = connection.Close() }()
	if _, err := connection.Write(request); err != nil {
		t.Fatalf("write LDAP bind: %v", err)
	}
	got := make([]byte, len(response))
	if _, err := io.ReadFull(connection, got); err != nil {
		t.Fatalf("read LDAP response: %v", err)
	}
	if string(got) != string(response) {
		t.Fatalf("LDAP response = %x, want %x", got, response)
	}
	fixture.assert(t, spec)
}
