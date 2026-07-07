package loggly

import (
	"net"
	"strings"
	"testing"
	"time"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestPostInitSetsDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{CustomerToken: "token"})

	if p.config.Severity != "INFO" {
		t.Fatalf("severity = %q, want INFO", p.config.Severity)
	}
	if len(p.config.Tags) != 1 || p.config.Tags[0] != "apisix" {
		t.Fatalf("tags = %v, want [apisix]", p.config.Tags)
	}
	if p.config.Host != "logs-01.loggly.com" {
		t.Fatalf("host = %q, want logs-01.loggly.com", p.config.Host)
	}
	if p.config.Port != 514 {
		t.Fatalf("port = %d, want 514", p.config.Port)
	}
}

func TestBuildMessageUsesRFC5424ShapeAndTags(t *testing.T) {
	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Severity:      "INFO",
		Tags:          []string{"apisix", "route-a"},
	})

	message := p.buildMessage(map[string]any{
		"status": 200,
		"path":   "/get",
	})

	if !strings.HasPrefix(message, "<14>1 ") {
		t.Fatalf("message = %q, want INFO priority prefix <14>1", message)
	}
	if !strings.Contains(message, `[token@41058 tag="apisix" tag="route-a"]`) {
		t.Fatalf("message = %q, want Loggly structured data with tags", message)
	}
	if !strings.Contains(message, `"path":"/get"`) {
		t.Fatalf("message = %q, want JSON log payload", message)
	}
}

func TestBuildMessageUsesSeverityMap(t *testing.T) {
	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Severity:      "INFO",
		SeverityMap:   map[string]string{"503": "ERR"},
	})

	message := p.buildMessage(map[string]any{"status": 503})
	if !strings.HasPrefix(message, "<11>1 ") {
		t.Fatalf("message = %q, want ERR priority prefix <11>1", message)
	}
}

func TestSendWritesUDPMessage(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          host,
		Port:          mustAtoi(t, port),
		Timeout:       1000,
	})

	p.Send(map[string]any{"status": 200, "path": "/get"})

	select {
	case message := <-received:
		if !strings.Contains(message, `[token@41058 tag="apisix"]`) {
			t.Fatalf("message = %q, want Loggly token and default tag", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for UDP log message")
	}
}

func startUDPServer(t *testing.T) (string, <-chan string) {
	t.Helper()

	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve udp addr: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
	})

	received := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _, err := conn.ReadFromUDP(buf)
		if err == nil {
			received <- string(buf[:n])
		}
	}()

	return conn.LocalAddr().String(), received
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()

	var n int
	for _, r := range value {
		if r < '0' || r > '9' {
			t.Fatalf("invalid integer %q", value)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
