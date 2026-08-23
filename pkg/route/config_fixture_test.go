package route

import (
	"os"
	"path/filepath"
	"time"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
)

func testEffectiveConfig() *appconfig.EffectiveConfig {
	static := appconfig.Config{
		CompatibilityTarget: appconfig.CompatibilityAPISIX317,
		SecurityProfile:     appconfig.SecurityCompat,
		Apisix: appconfig.Apisix{
			NodeListen: []appconfig.NodeListen{{Port: 9080}},
			ProxyMode:  "http",
		},
		NginxConfig: appconfig.NginxConfig{HTTP: appconfig.NginxHTTP{
			ClientMaxBodySize: 10 * 1024 * 1024,
			ClientBodyTimeout: 60 * time.Second,
		}},
		Proxy: appconfig.Proxy{
			MaxIdleConns: 1024, MaxIdleConnsPerHost: 256,
			MaxConnsPerHost: 512, MaxInFlight: 1024,
		},
		Plugins: []string{"request-id"},
		Deployment: appconfig.Deployment{
			Role:          "data_plane",
			RoleDataPlane: appconfig.RoleConfig{ConfigProvider: "yaml"},
		},
	}
	root := filepath.Join(os.TempDir(), "apisix-go-route-test")
	return &appconfig.EffectiveConfig{
		Config: static,
		Profiles: appconfig.ProfileSelection{
			Compatibility: appconfig.CompatibilityAPISIX317,
			Security:      appconfig.SecurityCompat,
		},
		Paths: appconfig.RuntimePaths{
			DataDir: filepath.Join(root, "data"), RuntimeDir: filepath.Join(root, "run"),
			LogDir: filepath.Join(root, "log"), TempDir: filepath.Join(root, "tmp"),
		},
	}
}

func testDataEncryptionResolver() data_encryption.Resolver {
	return data_encryption.NewResolver(false, nil)
}

func testDataEncryptionService() data_encryption.Service {
	return data_encryption.NewService(false, nil)
}
