package wolf_rbac

import (
	"net/http"
	"testing"
)

// BenchmarkStaticConfigPath measures the per-request wolf-rbac client
// selection. With ssl_verify disabled the insecure client must be built once
// at configuration time; requests must never clone the transport.
func BenchmarkStaticConfigPath(b *testing.B) {
	sslVerify := false
	p := newBenchmarkPlugin(b, Config{Server: "https://wolf.example.com", SSLVerify: &sslVerify})

	cfg := consumerConfig{SSLVerify: &sslVerify}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		client := p.clientForConfig(cfg)
		if client == nil {
			b.Fatal("clientForConfig() returned nil client")
		}
	}
}

func newBenchmarkPlugin(b testing.TB, cfg Config) *Plugin {
	b.Helper()
	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}
	b.Cleanup(func() {
		if transport, ok := p.client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	})
	return p
}
