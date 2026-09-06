package chaitin_waf

import (
	"encoding/json"
	"testing"
)

func TestParityRouteConfigReplacesMetadataTimeouts(t *testing.T) {
	for _, raw := range []string{`{"config":{"req_body_size":7}}`, `{"config":{}}`, `{}`} {
		t.Run(raw, func(t *testing.T) {
			var cfg Config
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
				t.Fatal(err)
			}
			p := newTestPluginWithMetadata(
				t,
				cfg,
				&Metadata{
					Config: WAFConfig{
						ReadTimeout:      25,
						SendTimeout:      35,
						ConnectTimeout:   45,
						ReqBodySize:      9,
						KeepaliveSize:    5,
						KeepaliveTimeout: 55,
						RealClientIP:     new(false),
					},
				},
			)
			wantTimeout := 1000
			if raw == `{}` {
				wantTimeout = 25
			}
			if p.effective.Config.ReadTimeout != wantTimeout {
				t.Fatalf("read timeout=%d want=%d", p.effective.Config.ReadTimeout, wantTimeout)
			}
			if *p.effective.Config.RealClientIP {
				t.Fatal("omitted route real_client_ip must retain explicit metadata false")
			}
		})
	}
}
