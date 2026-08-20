package route

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestBuildReverseHandlerValidatesUpstreamNodeWeights(t *testing.T) {
	tests := []struct {
		name      string
		config    string
		wantErr   string
		wantNoErr bool
	}{
		{
			name:    "missing list weight",
			config:  `{"nodes":[{"host":"127.0.0.1","port":8080}]}`,
			wantErr: "weight is required",
		},
		{
			name:    "negative list weight",
			config:  `{"nodes":[{"host":"127.0.0.1","port":8080,"weight":-1}]}`,
			wantErr: "weight must be non-negative",
		},
		{
			name:    "negative map weight",
			config:  `{"nodes":{"127.0.0.1:8080":-1}}`,
			wantErr: "weight must be non-negative",
		},
		{
			name: "explicit zero with positive target",
			config: `{"nodes":[
				{"host":"127.0.0.1","port":8080,"weight":0},
				{"host":"127.0.0.2","port":8080,"weight":1}
			]}`,
			wantNoErr: true,
		},
		{
			name:    "all zero list weights",
			config:  `{"nodes":[{"host":"127.0.0.1","port":8080,"weight":0}]}`,
			wantErr: "at least one upstream node must have a positive weight",
		},
		{
			name:    "all zero map weights",
			config:  `{"nodes":{"127.0.0.1:8080":0,"127.0.0.2:8080":0}}`,
			wantErr: "at least one upstream node must have a positive weight",
		},
		{
			name: "duplicate address leaves zero final weight",
			config: `{"nodes":[
				{"host":"127.0.0.1","port":8080,"weight":1},
				{"host":"[127.0.0.1]","port":8080,"weight":0}
			]}`,
			wantErr: "at least one upstream node must have a positive weight",
		},
		{
			name:      "empty nodes",
			config:    `{"nodes":[]}`,
			wantNoErr: true,
		},
		{
			name:      "positive map weight",
			config:    `{"nodes":{"127.0.0.1:8080":1}}`,
			wantNoErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var upstream resource.Upstream
			if err := json.Unmarshal([]byte(test.config), &upstream); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}

			builder := &Builder{}
			t.Cleanup(builder.Stop)
			_, err := builder.buildReverseHandler(resource.Route{Upstream: upstream}, resource.Service{})
			if test.wantNoErr {
				if err != nil {
					t.Fatalf("buildReverseHandler() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("buildReverseHandler() error = nil")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("buildReverseHandler() error = %q, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestBuildReverseHandlerAppliesSchemeAwareDefaultNodePorts(t *testing.T) {
	for _, test := range []struct {
		name   string
		config string
	}{
		{name: "http map", config: `{"scheme":"http","nodes":{"127.0.0.1":1}}`},
		{name: "https map", config: `{"scheme":"https","nodes":{"127.0.0.1":1}}`},
		{name: "grpc list", config: `{"scheme":"grpc","nodes":[{"host":"127.0.0.1","weight":1}]}`},
		{name: "grpcs list", config: `{"scheme":"grpcs","nodes":[{"host":"127.0.0.1","weight":1}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var upstream resource.Upstream
			if err := json.Unmarshal([]byte(test.config), &upstream); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if upstream.Nodes[0].Port != 0 {
				t.Fatalf("decoded port = %d, want zero for builder-owned default", upstream.Nodes[0].Port)
			}

			builder := &Builder{}
			t.Cleanup(builder.Stop)
			if _, err := builder.buildReverseHandler(
				resource.Route{Upstream: upstream},
				resource.Service{},
			); err != nil {
				t.Fatalf("buildReverseHandler() error = %v", err)
			}
		})
	}
}
