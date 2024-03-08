module github.com/wklken/apisix-go

go 1.21

replace github.com/coreos/bbolt => go.etcd.io/bbolt v1.3.9

// replace google.golang.org/grpc => google.golang.org/grpc v1.29.0

require (
	github.com/go-chi/chi/v5 v5.0.12
	github.com/goccy/go-json v0.10.2
	github.com/gofrs/uuid v4.4.0+incompatible
	github.com/justinas/alice v1.2.0
	github.com/oxtoacart/bpool v0.0.0-20190530202638-03653db5a59c
	github.com/santhosh-tekuri/jsonschema/v5 v5.3.1
	github.com/smallnest/weighted v0.0.0-20230419055410-36b780e40a7a
	github.com/spf13/cobra v1.8.0
	github.com/spf13/viper v1.18.2
	github.com/unrolled/render v1.6.1
	go.etcd.io/bbolt v1.3.4
	go.etcd.io/etcd/client/v3 v3.5.10
	go.uber.org/zap v1.27.0
	golang.org/x/net v0.21.0
)

require (
	github.com/coreos/go-semver v0.3.1 // indirect
	github.com/coreos/go-systemd/v22 v22.3.2 // indirect
	github.com/fsnotify/fsnotify v1.7.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/hashicorp/hcl v1.0.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/magiconair/properties v1.8.7 // indirect
	github.com/mitchellh/mapstructure v1.5.0 // indirect
	github.com/pelletier/go-toml/v2 v2.1.0 // indirect
	github.com/sagikazarmark/locafero v0.4.0 // indirect
	github.com/sagikazarmark/slog-shim v0.1.0 // indirect
	github.com/sourcegraph/conc v0.3.0 // indirect
	github.com/spf13/afero v1.11.0 // indirect
	github.com/spf13/cast v1.6.0 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	go.etcd.io/etcd/api/v3 v3.5.10 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.5.10 // indirect
	go.uber.org/multierr v1.10.0 // indirect
	golang.org/x/exp v0.0.0-20230905200255-921286631fa9 // indirect
	golang.org/x/sys v0.18.0 // indirect
	golang.org/x/text v0.14.0 // indirect
	google.golang.org/genproto v0.0.0-20231106174013-bbf56f31fb17 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20231106174013-bbf56f31fb17 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20231120223509-83a465c0220f // indirect
	google.golang.org/grpc v1.59.0 // indirect
	google.golang.org/protobuf v1.32.0 // indirect
	gopkg.in/ini.v1 v1.67.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)