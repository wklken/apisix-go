package route

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/dubbo_proxy"
	"github.com/wklken/apisix-go/pkg/plugin/http_dubbo"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestHTTPUpstreamAdmitsAPISIXAlgorithms(t *testing.T) {
	for _, algorithm := range []string{"chash", "least_conn", "ewma"} {
		t.Run(algorithm, func(t *testing.T) {
			upstream := resource.Upstream{Type: algorithm, HashOn: "header", Key: "X-Tenant"}
			if err := validateHTTPUpstreamType(upstream); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHTTPUpstreamAlgorithmsForwardThroughPreparedRoute(t *testing.T) {
	first := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "first") }),
	)
	defer first.Close()
	second := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "second") }),
	)
	defer second.Close()
	for _, algorithm := range []string{"chash", "least_conn", "ewma"} {
		t.Run(algorithm, func(t *testing.T) {
			route := testRouteFromJSON(
				t,
				fmt.Sprintf(
					`{"id":"algorithm","uri":"/","upstream":{"type":%q,"hash_on":"header","key":"X-Tenant","nodes":{%q:1,%q:1}}}`,
					algorithm,
					first.Listener.Addr().String(),
					second.Listener.Addr().String(),
				),
			)
			handler := testPreparedProxyHandler(t, route, resource.Service{}, testEffectiveConfig())
			selected := make(map[string]string)
			seen := make(map[string]bool)
			for i := range 32 {
				key := fmt.Sprint(i / 2)
				request := httptest.NewRequest(http.MethodGet, "http://gateway/", nil)
				request.Header.Set("X-Tenant", key)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != 200 {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				body := response.Body.String()
				if body != "first" && body != "second" {
					t.Fatalf("body=%s", body)
				}
				if previous, ok := selected[key]; ok && algorithm == "chash" && previous != body {
					t.Fatalf("key %s switched %s -> %s", key, previous, body)
				}
				selected[key] = body
				seen[body] = true
			}
			if algorithm == "chash" && len(seen) != 2 {
				t.Fatal("different hash keys did not reach both endpoints")
			}
		})
	}
}

func TestDubboTerminalsReleaseLeastConnectionSelection(t *testing.T) {
	for _, protocol := range []string{"dubbo", "http-dubbo"} {
		t.Run(protocol, func(t *testing.T) {
			var upstream string
			if protocol == "dubbo" {
				upstream = startRouteHessianDubboTestServer(t)
			} else {
				upstream = startRouteDubboTestServer(t, routeDubboFrame("1\nfrom route upstream\n"))
			}
			live := "dubbo://" + upstream
			cluster, err := pxy.NewCluster(
				pxy.ClusterConfig{Type: "least_conn", Targets: map[string]int{live: 2, "dubbo://127.0.0.1:1": 1}},
				pxy.NopClusterObserver{},
			)
			if err != nil {
				t.Fatal(err)
			}
			defer cluster.Close()
			request := httptest.NewRequest(http.MethodPost, "http://gateway/dubbo", nil)
			response := httptest.NewRecorder()
			if protocol == "dubbo" {
				request = dubbo_proxy.WithConfig(
					request,
					dubbo_proxy.Config{ServiceName: "svc", ServiceVersion: "1.0.0", Method: "hello"},
				)
				serveDubboIfConfiguredCompiled(response, request, cluster.LoadBalancer(), nil)
			} else {
				request = http_dubbo.WithConfig(
					request,
					http_dubbo.Config{ServiceName: "svc", ServiceVersion: "0.0.0", Method: "hello"},
				)
				serveHTTPDubboIfConfiguredCompiled(response, request, cluster.LoadBalancer(), nil)
			}
			if response.Code != 200 {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if next := cluster.LoadBalancer().Next(); next != live {
				t.Fatalf("completed RPC still counts as active; next=%s want=%s", next, live)
			}
		})
	}
}

func TestHTTPUpstreamEmptyHashTemplatePreservesPresence(t *testing.T) {
	for _, keyField := range []string{`,"key":""`, ""} {
		var upstream resource.Upstream
		if err := json.Unmarshal(
			[]byte(`{"type":"chash","hash_on":"vars_combinations","nodes":{"127.0.0.1:1":1}`+keyField+`}`),
			&upstream,
		); err != nil {
			t.Fatal(err)
		}
		err := validateHTTPUpstreamType(upstream)
		if keyField == "" {
			if err == nil {
				t.Fatal("missing hash key admitted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		config, err := buildClusterConfigWithSSLResolver(
			resource.Route{},
			upstream,
			map[string]int{"http://127.0.0.1:1": 1},
			nil,
			&testEffectiveConfig().Config,
		)
		if err != nil {
			t.Fatal(err)
		}
		cluster, err := pxy.NewCluster(config, pxy.NopClusterObserver{})
		if err != nil {
			t.Fatal(err)
		}
		cluster.Close()
	}
}
