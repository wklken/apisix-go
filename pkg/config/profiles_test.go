package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

func TestProfileSelectionValidate(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		in      ProfileSelection
		wantErr string
	}{
		{
			name: "compat without qualification",
			in: ProfileSelection{
				Compatibility: CompatibilityAPISIX317,
				Security:      SecurityCompat,
			},
		},
		{
			name: "strict is independent",
			in: ProfileSelection{
				Compatibility: CompatibilityAPISIX317,
				Security:      SecurityStrict,
			},
		},
		{
			name: "known but not yet qualified",
			in: ProfileSelection{
				Compatibility: CompatibilityAPISIX317,
				Security:      SecurityStrict,
				Qualification: QualificationHTTPDataPlaneV1,
			},
			wantErr: "unqualified required plugins",
		},
		{
			name: "unknown target",
			in: ProfileSelection{
				Compatibility: "apisix-master",
				Security:      SecurityCompat,
			},
			wantErr: "compatibility_target",
		},
		{
			name: "unknown security",
			in: ProfileSelection{
				Compatibility: CompatibilityAPISIX317,
				Security:      "unsafe",
			},
			wantErr: "security_profile",
		},
		{
			name: "unknown qualification",
			in: ProfileSelection{
				Compatibility: CompatibilityAPISIX317,
				Security:      SecurityCompat,
				Qualification: "future",
			},
			wantErr: "qualification_profile",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.in.Validate(manifest)
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("Validate() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestProfileSelectionConfigAxes(t *testing.T) {
	cfg := &Config{
		CompatibilityTarget:  CompatibilityAPISIX317,
		SecurityProfile:      SecurityStrict,
		QualificationProfile: QualificationHTTPDataPlaneV1,
	}
	want := ProfileSelection{
		Compatibility: CompatibilityAPISIX317,
		Security:      SecurityStrict,
		Qualification: QualificationHTTPDataPlaneV1,
	}
	if got := cfg.Profiles(); got != want {
		t.Fatalf("Profiles() = %#v, want %#v", got, want)
	}
}

func TestProfileSelectionRuntimePoliciesAreOrthogonal(t *testing.T) {
	manifest := loadProfileTestManifest(t)

	t.Run("compat security preserves APISIX transport defaults", func(t *testing.T) {
		cfg := validProfileSelectionConfig()
		cfg.SecurityProfile = SecurityCompat
		cfg.QualificationProfile = QualificationNone
		cfg.Debug = true
		cfg.Apisix.TrustedAddresses = []string{"10.0.0.1"}
		cfg.Deployment.Etcd.Host = []string{"http://etcd.example:2379"}
		cfg.Deployment.Etcd.TLS.Verify = nil

		if err := validateRuntimeConfig(cfg, manifest); err != nil {
			t.Fatalf("validateRuntimeConfig() error = %v, want compatibility security defaults accepted", err)
		}
	})

	t.Run("strict security applies without qualification", func(t *testing.T) {
		cfg := validProfileSelectionConfig()
		cfg.QualificationProfile = QualificationNone
		cfg.Plugins = []string{"request-id"}
		cfg.Debug = true

		err := validateRuntimeConfig(cfg, manifest)
		if err == nil || !strings.Contains(err.Error(), "strict") || !strings.Contains(err.Error(), "debug") {
			t.Fatalf("validateRuntimeConfig() error = %v, want strict debug rejection", err)
		}
	})

	t.Run("HTTP qualification applies under compatibility security", func(t *testing.T) {
		cfg := validProfileSelectionConfig()
		cfg.SecurityProfile = SecurityCompat
		cfg.Debug = true
		cfg.Apisix.TrustedAddresses = nil
		cfg.Deployment.Etcd.Host = []string{"http://etcd.example:2379"}
		cfg.Deployment.Etcd.TLS.Verify = nil

		if err := validateRuntimeConfig(cfg, qualifiedProfileTestManifest(t)); err != nil {
			t.Fatalf("validateRuntimeConfig() error = %v, want HTTP qualification independent of strict security", err)
		}
	})
}

func TestHTTPDataPlaneQualificationUsesManifestPluginContract(t *testing.T) {
	manifest := qualifiedProfileTestManifest(t)
	qualification, ok := manifest.Qualification(string(QualificationHTTPDataPlaneV1))
	if !ok {
		t.Fatal("HTTP qualification missing")
	}
	if !slices.Equal(manifest.QualifiedPlugins(string(QualificationHTTPDataPlaneV1)), qualification.RequiredPlugins) {
		t.Fatal("test manifest does not qualify every required plugin")
	}

	cfg := validProfileSelectionConfig()
	if err := validateRuntimeConfig(cfg, manifest); err != nil {
		t.Fatalf("validateRuntimeConfig() error = %v, want valid manifest-derived plugin contract", err)
	}

	cfg.Plugins[0], cfg.Plugins[1] = cfg.Plugins[1], cfg.Plugins[0]
	err := validateRuntimeConfig(cfg, manifest)
	want := "qualification_profile http-data-plane-v1: plugins must exactly match required order"
	if err == nil || err.Error() != want {
		t.Fatalf("validateRuntimeConfig() error = %v, want %q", err, want)
	}
}

func loadProfileTestManifest(t *testing.T) *capability.Manifest {
	t.Helper()
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func qualifiedProfileTestManifest(t *testing.T) *capability.Manifest {
	t.Helper()
	manifest := loadProfileTestManifest(t)
	index := slices.IndexFunc(manifest.QualificationProfiles, func(profile capability.QualificationProfile) bool {
		return profile.Name == string(QualificationHTTPDataPlaneV1)
	})
	if index < 0 {
		t.Fatalf("qualification profiles = %#v, want %q", manifest.QualificationProfiles, QualificationHTTPDataPlaneV1)
	}
	manifest.QualificationProfiles[index].RequiredEvidence = nil
	return manifest
}

func validProfileSelectionConfig() *Config {
	verify := true
	return &Config{
		CompatibilityTarget:  CompatibilityAPISIX317,
		SecurityProfile:      SecurityStrict,
		QualificationProfile: QualificationHTTPDataPlaneV1,
		Apisix: Apisix{
			NodeListen:       []NodeListen{{Ip: "0.0.0.0", Port: 9080}},
			ProxyMode:        "http",
			Ssl:              Ssl{Enable: true, Listen: []Listen{{Ip: "0.0.0.0", Port: 9443, EnableHttp2: true}}},
			TrustedAddresses: []string{"10.0.0.0/8"},
		},
		NginxConfig: NginxConfig{HTTP: NginxHTTP{
			ClientBodyTimeout: 60,
			ClientMaxBodySize: 10 * 1024 * 1024,
		}},
		Plugins: []string{"basic-auth", "cors", "jwt-auth", "key-auth", "prometheus", "request-id"},
		Proxy:   Proxy{MaxIdleConns: 1024, MaxIdleConnsPerHost: 256, MaxConnsPerHost: 512, MaxInFlight: 1024},
		Deployment: Deployment{
			Role:          "data_plane",
			RoleDataPlane: RoleConfig{ConfigProvider: "etcd"},
			Etcd: Etcd{
				Host:   []string{"https://etcd.example:2379"},
				Prefix: "/apisix",
				TLS:    EtcdTLS{Verify: &verify},
			},
		},
	}
}
