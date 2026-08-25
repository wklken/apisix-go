package route

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

type preparedHandlerTestPlugin struct {
	name     string
	priority int
	handler  func(http.Handler) http.Handler
}

func TestBuildPreparedHandlerResolvesOnlyPreparedConsumerRecords(t *testing.T) {
	t.Parallel()

	consumerBinding, err := plugin.BindPluginChecked(
		"proxy-rewrite",
		&preparedHandlerTestPlugin{
			name:     "proxy-rewrite",
			priority: 1008,
			handler: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					consumerName, _ := apisixctx.GetApisixVar(request, "$consumer_name").(string)
					groupID, _ := apisixctx.GetApisixVar(request, "$consumer_group_id").(string)
					override := "missing"
					if apisixctx.ConsumerPluginOverrides(request, "proxy-rewrite") {
						override = "present"
					}
					request.Header.Set(
						"X-Prepared-Consumer",
						consumerName+"|"+groupID+"|"+override,
					)
					next.ServeHTTP(writer, request)
				})
			},
		},
		plugin.ScopeConsumer,
		plugin.ResourceProvenance{Kind: plugin.ResourceConsumer, ID: "alice"},
	)
	if err != nil {
		t.Fatalf("BindPluginChecked() error = %v", err)
	}

	const target = "http://consumer-upstream.test:8080"
	consumers := map[string]PreparedConsumerRecord{
		"alice": {
			Consumer: resource.Consumer{Username: "alice", GroupID: "prepared-group"},
			Bindings: []plugin.Binding{consumerBinding},
		},
	}
	handler, err := BuildPreparedHandler(PreparedHandlerInput{
		Route:     resource.Route{ID: "prepared-consumer-route", Uri: "/consumer"},
		Consumers: consumers,
		Upstream: UpstreamPlan{
			Upstream: resource.Upstream{Scheme: "http", Nodes: []resource.Node{{
				Host: "consumer-upstream.test", Port: 8080, Weight: 1,
			}}},
			Provenance: plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "prepared-consumer-route"},
			Targets:    map[string]int{target: 1},
		},
		Runtime: PreparedUpstreamRuntime{
			LoadBalancer: proxy.NewSingleLoadBalance(target),
			RoundTripper: preparedHandlerRoundTripper(func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(
						request.Header.Get("X-Prepared-Consumer") + "|" +
							request.Header.Get("X-Consumer-Username"),
					)),
					Request: request,
				}, nil
			}),
		},
	})
	if err != nil {
		t.Fatalf("BuildPreparedHandler() error = %v", err)
	}

	// The published handler owns both the Consumer and the binding slice.
	consumers["alice"] = PreparedConsumerRecord{Consumer: resource.Consumer{Username: "mutated"}}
	delete(consumers, "alice")

	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/consumer", nil)
	request = apisixctx.WithAuthenticationState(request, apisixctx.NewAuthenticationState(
		"jwt-auth",
		resource.Consumer{Username: "alice", GroupID: "untrusted-request-group"},
	))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", response.Code, http.StatusOK, response.Body.String())
	}
	if got, want := response.Body.String(), "alice|prepared-group|present|alice"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestBuildPreparedHandlerFailsClosedForUnpreparedAuthenticatedConsumer(t *testing.T) {
	t.Parallel()

	const target = "http://missing-consumer-upstream.test:8080"
	handler, err := BuildPreparedHandler(PreparedHandlerInput{
		Route: resource.Route{ID: "missing-consumer-route", Uri: "/consumer"},
		Upstream: UpstreamPlan{
			Upstream: resource.Upstream{Scheme: "http", Nodes: []resource.Node{{
				Host: "missing-consumer-upstream.test", Port: 8080, Weight: 1,
			}}},
			Provenance: plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "missing-consumer-route"},
			Targets:    map[string]int{target: 1},
		},
		Runtime: PreparedUpstreamRuntime{
			LoadBalancer: proxy.NewSingleLoadBalance(target),
			RoundTripper: preparedHandlerRoundTripper(func(request *http.Request) (*http.Response, error) {
				t.Fatalf("unprepared authenticated consumer reached upstream: %s", request.URL)
				return nil, nil
			}),
		},
	})
	if err != nil {
		t.Fatalf("BuildPreparedHandler() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/consumer", nil)
	request = apisixctx.WithAuthenticationState(request, apisixctx.NewAuthenticationState(
		"key-auth",
		resource.Consumer{Username: "missing"},
	))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	if got := strings.TrimSpace(response.Body.String()); got != `{"message":"Internal Server Error"}` {
		t.Fatalf("body = %q, want stable internal error", got)
	}
}

func TestBuildPreparedHandlerAssemblesPreparedKafkaProtocolTerminal(t *testing.T) {
	t.Parallel()

	handler, err := BuildPreparedHandler(PreparedHandlerInput{
		Route: resource.Route{ID: "prepared-kafka-route", Uri: "/kafka"},
		Upstream: UpstreamPlan{
			Upstream: resource.Upstream{
				Scheme: "kafka",
				Nodes:  []resource.Node{{Host: "broker.test", Port: 9092, Weight: 1}},
			},
			Provenance: plugin.ResourceProvenance{
				Kind: plugin.ResourceRoute,
				ID:   "prepared-kafka-route",
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildPreparedHandler() error = %v", err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "http://gateway.test/kafka", nil),
	)
	if response.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUpgradeRequired)
	}
}

func TestBuildPreparedHandlerEnforcesPreparedWebsocketPolicy(t *testing.T) {
	t.Parallel()

	const target = "http://websocket-upstream.test:8080"
	handler, err := BuildPreparedHandler(PreparedHandlerInput{
		Route: resource.Route{ID: "prepared-websocket-route", Uri: "/socket"},
		Upstream: UpstreamPlan{
			Upstream: resource.Upstream{Scheme: "http", Nodes: []resource.Node{{
				Host: "websocket-upstream.test", Port: 8080, Weight: 1,
			}}},
			Provenance: plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "prepared-websocket-route"},
			Targets:    map[string]int{target: 1},
		},
		Runtime: PreparedUpstreamRuntime{
			LoadBalancer: proxy.NewSingleLoadBalance(target),
			RoundTripper: preparedHandlerRoundTripper(func(request *http.Request) (*http.Response, error) {
				t.Fatalf("disabled websocket reached prepared upstream: %s", request.URL)
				return nil, nil
			}),
		},
	})
	if err != nil {
		t.Fatalf("BuildPreparedHandler() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/socket", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestPreparedHandlerBoundaryHasNoLifecycleOrConstructionAuthority(t *testing.T) {
	t.Parallel()

	runtimeType := reflect.TypeFor[PreparedUpstreamRuntime]()
	if runtimeType.NumMethod() != 0 {
		t.Fatalf("PreparedUpstreamRuntime methods = %d, want no lifecycle methods", runtimeType.NumMethod())
	}
	for field := range runtimeType.Fields() {
		if field.Type == reflect.TypeFor[*proxy.Cluster]() {
			t.Fatalf("PreparedUpstreamRuntime.%s exposes *proxy.Cluster lifecycle authority", field.Name)
		}
	}

	filename := filepath.Join("prepared_handler.go")
	parsed, err := parser.ParseFile(token.NewFileSet(), filename, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	for _, imported := range parsed.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			t.Fatalf("unquote import %s: %v", imported.Path.Value, err)
		}
		forbidden := strings.HasSuffix(path, "/store") ||
			strings.HasSuffix(path, "/runtime") ||
			strings.HasSuffix(path, "/secret") ||
			strings.Contains(path, "/task")
		if forbidden {
			t.Fatalf("prepared handler imports authority package %q", path)
		}
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		owner, _ := selector.X.(*ast.Ident)
		qualified := ""
		if owner != nil {
			qualified = owner.Name + "." + selector.Sel.Name
		}
		switch qualified {
		case "pxy.NewCluster", "pxy.NewClusterRegistry", "pxy.NewTransport",
			"pxy.NewRetryTransport", "plugin.New":
			t.Errorf("prepared handler calls forbidden constructor %s", qualified)
		}
		if selector.Sel.Name == "Acquire" || selector.Sel.Name == "Close" {
			t.Errorf("prepared handler calls forbidden lifecycle method %s", selector.Sel.Name)
		}
		return true
	})
}

func TestBuildPreparedNotFoundHandlerRunsPreparedSystemAndGlobalBindings(t *testing.T) {
	t.Parallel()

	order := make([]string, 0, 3)
	system, err := plugin.BindPluginChecked(
		"request-context",
		&preparedHandlerTestPlugin{
			name:     "request-context",
			priority: 12000,
			handler: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					order = append(order, "system")
					next.ServeHTTP(writer, request)
				})
			},
		},
		plugin.ScopeSystem,
		plugin.ResourceProvenance{Kind: plugin.ResourceSystem, ID: "request-context"},
	)
	if err != nil {
		t.Fatalf("bind system plugin: %v", err)
	}
	global, err := plugin.BindPluginChecked(
		"proxy-rewrite",
		&preparedHandlerTestPlugin{
			name:     "proxy-rewrite",
			priority: 1008,
			handler: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					order = append(order, "global")
					next.ServeHTTP(writer, request)
					source := apisixctx.ResponseSourceUnknown
					if lifecycle := apisixctx.GetRequestLifecycle(request); lifecycle != nil {
						source = lifecycle.ResponseSource()
					}
					order = append(order, "source:"+string(source))
				})
			},
		},
		plugin.ScopeGlobal,
		plugin.ResourceProvenance{Kind: plugin.ResourceGlobalRule, ID: "global-404"},
	)
	if err != nil {
		t.Fatalf("bind global plugin: %v", err)
	}

	handler, err := BuildPreparedNotFoundHandler([]plugin.Binding{global, system})
	if err != nil {
		t.Fatalf("BuildPreparedNotFoundHandler() error = %v", err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://gateway.test/missing", nil))

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
	if got, want := strings.Join(order, ","), "system,global,source:early_stop"; got != want {
		t.Fatalf("execution order = %q, want %q", got, want)
	}
}

func (*preparedHandlerTestPlugin) Init() error               { return nil }
func (*preparedHandlerTestPlugin) PostInit() error           { return nil }
func (*preparedHandlerTestPlugin) Config() any               { return nil }
func (*preparedHandlerTestPlugin) GetSchema() string         { return "" }
func (*preparedHandlerTestPlugin) GetMetadataSchema() string { return "" }
func (p *preparedHandlerTestPlugin) GetPriority() int        { return p.priority }
func (p *preparedHandlerTestPlugin) GetName() string         { return p.name }
func (p *preparedHandlerTestPlugin) Handler(next http.Handler) http.Handler {
	return p.handler(next)
}

type preparedHandlerRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip preparedHandlerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestBuildPreparedHandlerUsesOnlyPreparedBindingsAndRuntime(t *testing.T) {
	t.Parallel()

	binding, err := plugin.BindPluginChecked(
		"request-id",
		&preparedHandlerTestPlugin{
			name:     "request-id",
			priority: 12015,
			handler: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					request.Header.Set("X-Prepared-Binding", "ran")
					next.ServeHTTP(writer, request)
				})
			},
		},
		plugin.ScopeRoute,
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "prepared-route"},
	)
	if err != nil {
		t.Fatalf("BindPluginChecked() error = %v", err)
	}

	const target = "http://upstream.test:8080"
	runtime := PreparedUpstreamRuntime{
		LoadBalancer: proxy.NewSingleLoadBalance(target),
		RoundTripper: preparedHandlerRoundTripper(func(request *http.Request) (*http.Response, error) {
			body := request.URL.String() + "|" + request.Host + "|" + request.Header.Get("X-Prepared-Binding")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    request,
			}, nil
		}),
	}
	input := PreparedHandlerInput{
		Route:          resource.Route{ID: "prepared-route", Uri: "/orders"},
		StaticBindings: []plugin.Binding{binding},
		Upstream: UpstreamPlan{
			Upstream: resource.Upstream{
				Scheme: "http",
				Nodes:  []resource.Node{{Host: "upstream.test", Port: 8080, Weight: 1}},
			},
			Provenance: plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "prepared-route"},
			Targets:    map[string]int{target: 1},
		},
		Runtime:      runtime,
		StaticConfig: appconfig.Config{},
	}

	handler, err := BuildPreparedHandler(input)
	if err != nil {
		t.Fatalf("BuildPreparedHandler() error = %v", err)
	}

	// Mutating the caller-owned containers after assembly must not change the
	// detached generation.
	input.StaticBindings[0] = plugin.Binding{}
	delete(input.Upstream.Targets, target)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", response.Code, http.StatusOK, response.Body.String())
	}
	if got, want := response.Body.String(), "http://upstream.test:8080/orders|gateway.test|ran"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}
