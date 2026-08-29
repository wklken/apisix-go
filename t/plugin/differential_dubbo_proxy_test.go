package pluginintegration

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"maps"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/apache/dubbo-go-hessian2"
)

func TestDifferentialDubboProxyCaseMapsPinnedAPISIX317RouteTest3(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}
	cases := differentialDubboProxyCases()
	if len(cases) != 1 {
		t.Fatalf("differentialDubboProxyCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "dubbo-proxy-hessian2-http-context" || spec.Plugin != "dubbo-proxy" ||
		spec.RouteID != "differential-dubbo-proxy-hessian2-http-context" ||
		spec.ComparisonPolicy != differentialDubboProxyHessian2Policy {
		t.Fatalf("case identity = %q/%q/%q/%q", spec.Name, spec.Plugin, spec.RouteID, spec.ComparisonPolicy)
	}
	if spec.Request.Method != http.MethodPost || spec.Request.Path != "/hello" ||
		spec.Request.Host != differentialDubboProxyHTTPHost ||
		spec.Request.Headers["Extra-Arg-K"] != differentialDubboProxyHeaderValue ||
		spec.Request.Body != differentialDubboProxyRequestBody {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "dubbo-proxy" ||
		spec.Fixture.WireProtocol != differentialFixtureWireDubboProxyHessian2 ||
		spec.Fixture.ExpectedCalls != 1 || spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Headers["Got-extra-arg-k"] != differentialDubboProxyHeaderValue ||
		spec.Fixture.Response.Body != differentialDubboProxyResponseBody {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}

	routes := spec.Config["routes"].([]any)
	route := routes[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["dubbo-proxy"].(map[string]any)
	if pluginConfig["service_name"] != differentialDubboProxyServiceName ||
		pluginConfig["service_version"] != differentialDubboProxyServiceVersion ||
		pluginConfig["method"] != differentialDubboProxyMethodName {
		t.Fatalf("plugin config = %#v", pluginConfig)
	}
	upstream := route["upstream"].(map[string]any)
	if upstream["type"] != "roundrobin" ||
		upstream["nodes"].(map[string]any)[differentialFixturePlaceholder] != 1 {
		t.Fatalf("upstream = %#v", upstream)
	}
}

func TestDifferentialDubboProxyFixtureCapturesHessian2ContextAndResponds(t *testing.T) {
	spec := differentialDubboProxyCases()[0].Fixture
	fixture, err := startDifferentialDubboProxyFixture(spec)
	if err != nil {
		t.Fatalf("startDifferentialDubboProxyFixture() error = %v", err)
	}
	t.Cleanup(fixture.close)

	requestID := uint64(0x0102030405060708)
	frame := buildDifferentialDubboProxyRequestFrameForTest(t, requestID, differentialDubboProxyInvocation{
		ProtocolVersion: differentialDubboProxyProtocolVersion,
		ServiceName:     differentialDubboProxyServiceName,
		ServiceVersion:  differentialDubboProxyServiceVersion,
		Method:          differentialDubboProxyMethodName,
		ParamsTypeDesc:  differentialDubboProxyParamsTypeDesc,
		HTTPContext: map[string][]string{
			"host":        {differentialDubboProxyHTTPHost},
			"extra-arg-k": {differentialDubboProxyHeaderValue},
		},
		HTTPBody: differentialDubboProxyRequestBody,
	})
	connection, err := net.DialTimeout(
		"tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.port())), time.Second,
	)
	if err != nil {
		t.Fatalf("dial Dubbo fixture: %v", err)
	}
	defer func() { _ = connection.Close() }()
	if _, err := connection.Write(frame); err != nil {
		t.Fatalf("write Dubbo request: %v", err)
	}
	response, err := readDifferentialDubboProxyFrame(bufio.NewReader(connection))
	if err != nil {
		t.Fatalf("read Dubbo response: %v", err)
	}
	assertDifferentialDubboProxyResponseForTest(t, response, requestID)

	captured, err := fixture.collectWithTimeout(1, time.Second)
	if err != nil {
		t.Fatalf("collectWithTimeout() error = %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured calls = %d, want 1", len(captured))
	}
	call := captured[0]
	if call.Method != differentialDubboProxyWireMethod ||
		call.Path != differentialDubboProxyServiceName+"/"+differentialDubboProxyMethodName ||
		call.Host != differentialDubboProxyServiceVersion {
		t.Fatalf("captured method/path/host = %q/%q/%q", call.Method, call.Path, call.Host)
	}
	var invocation differentialDubboProxyInvocation
	if err := json.Unmarshal([]byte(call.Body), &invocation); err != nil {
		t.Fatalf("decode captured invocation: %v", err)
	}
	if invocation.RequestID != requestID || invocation.HTTPBody != differentialDubboProxyRequestBody ||
		invocation.HTTPContext["host"][0] != differentialDubboProxyHTTPHost ||
		invocation.HTTPContext["extra-arg-k"][0] != differentialDubboProxyHeaderValue {
		t.Fatalf("captured invocation = %#v", invocation)
	}
}

func TestCompareDifferentialDubboProxyHessian2NormalizesOnlyTransportEnvironment(t *testing.T) {
	spec := differentialDubboProxyCases()[0]
	candidate := differentialDubboProxyObservationForTest(
		t, spec, 101, "127.0.0.1:31001", "31001", false,
	)
	oracle := differentialDubboProxyObservationForTest(
		t, spec, 202, "host.containers.internal:32001", "9080", true,
	)
	passed, detail, err := compareDifferentialDubboProxyHessian2(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed {
		t.Fatalf("compare = %t, detail %q, error %v", passed, detail, err)
	}

	mutations := []struct {
		name   string
		mutate func(*differentialDubboProxyInvocation)
	}{
		{name: "service", mutate: func(v *differentialDubboProxyInvocation) { v.ServiceName = "wrong.Service" }},
		{name: "version", mutate: func(v *differentialDubboProxyInvocation) { v.ServiceVersion = "9.9.9" }},
		{name: "method", mutate: func(v *differentialDubboProxyInvocation) { v.Method = "wrong" }},
		{
			name:   "params descriptor",
			mutate: func(v *differentialDubboProxyInvocation) { v.ParamsTypeDesc = "Ljava/lang/String;" },
		},
		{
			name:   "HTTP header",
			mutate: func(v *differentialDubboProxyInvocation) { v.HTTPContext["extra-arg-k"] = []string{"wrong"} },
		},
		{
			name:   "HTTP host",
			mutate: func(v *differentialDubboProxyInvocation) { v.HTTPContext["host"] = []string{"wrong.example"} },
		},
		{
			name:   "missing user agent",
			mutate: func(v *differentialDubboProxyInvocation) { delete(v.HTTPContext, "user-agent") },
		},
		{
			name:   "invalid forwarded port",
			mutate: func(v *differentialDubboProxyInvocation) { v.HTTPContext["x-forwarded-port"] = []string{"not-a-port"} },
		},
		{
			name:   "unexpected connection mode",
			mutate: func(v *differentialDubboProxyInvocation) { v.HTTPContext["connection"] = []string{"upgrade"} },
		},
		{name: "HTTP body", mutate: func(v *differentialDubboProxyInvocation) { v.HTTPBody = "wrong" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := differentialDubboProxyObservationForTest(
				t, spec, 303, "127.0.0.1:31001", "31001", false,
			)
			var invocation differentialDubboProxyInvocation
			if err := json.Unmarshal([]byte(changed.Upstream.Body), &invocation); err != nil {
				t.Fatal(err)
			}
			mutation.mutate(&invocation)
			encoded, err := json.Marshal(invocation)
			if err != nil {
				t.Fatal(err)
			}
			changed.Upstream.Body = string(encoded)
			if passed, _, err := compareDifferentialDubboProxyHessian2(
				spec, changed, oracle, testNormalizationPolicy(),
			); err == nil || passed {
				t.Fatalf("mutated comparison = %t/%v, want rejection", passed, err)
			}
		})
	}
}

func buildDifferentialDubboProxyRequestFrameForTest(
	t *testing.T,
	requestID uint64,
	invocation differentialDubboProxyInvocation,
) []byte {
	t.Helper()
	contextMap := make(map[string]any, len(invocation.HTTPContext)+1)
	for name, values := range invocation.HTTPContext {
		if len(values) == 1 {
			contextMap[name] = values[0]
		} else {
			contextMap[name] = values
		}
	}
	contextMap["body"] = []byte(invocation.HTTPBody)
	encoder := hessian.NewEncoder()
	for _, value := range []any{
		invocation.ProtocolVersion,
		invocation.ServiceName,
		invocation.ServiceVersion,
		invocation.Method,
		invocation.ParamsTypeDesc,
		contextMap,
		nil,
	} {
		if err := encoder.Encode(value); err != nil {
			t.Fatalf("encode request value: %v", err)
		}
	}
	payload := encoder.Buffer()
	frame := make([]byte, 16+len(payload))
	frame[0], frame[1], frame[2] = 0xda, 0xbb, 0xc2
	binary.BigEndian.PutUint64(frame[4:12], requestID)
	binary.BigEndian.PutUint32(frame[12:16], uint32(len(payload)))
	copy(frame[16:], payload)
	return frame
}

func assertDifferentialDubboProxyResponseForTest(t *testing.T, frame []byte, requestID uint64) {
	t.Helper()
	if len(frame) < 16 || !bytes.Equal(frame[:2], []byte{0xda, 0xbb}) ||
		frame[2] != 0x02 || frame[3] != 20 || binary.BigEndian.Uint64(frame[4:12]) != requestID {
		t.Fatalf("response header = %x", frame[:min(len(frame), 16)])
	}
	decoder := hessian.NewDecoder(frame[16:])
	responseType, err := decoder.Decode()
	if err != nil || responseType != int32(1) {
		t.Fatalf("response type = %#v/%v", responseType, err)
	}
	value, err := decoder.Decode()
	if err != nil {
		t.Fatalf("decode response map: %v", err)
	}
	responseMap := differentialDubboProxyMapForTest(t, value)
	for name, want := range map[string]string{
		"status":          "200",
		"body":            differentialDubboProxyResponseBody,
		"Got-extra-arg-k": differentialDubboProxyHeaderValue,
	} {
		if got := responseMap[name]; got != want {
			t.Fatalf("response %s = %#v, want %q", name, got, want)
		}
	}
}

func differentialDubboProxyMapForTest(t *testing.T, value any) map[string]any {
	t.Helper()
	result := make(map[string]any)
	switch typed := value.(type) {
	case map[any]any:
		for key, item := range typed {
			name, ok := key.(string)
			if !ok {
				t.Fatalf("map key = %T, want string", key)
			}
			result[name] = item
		}
	case map[string]any:
		maps.Copy(result, typed)
	default:
		t.Fatalf("decoded map = %T", value)
	}
	return result
}

func differentialDubboProxyObservationForTest(
	t *testing.T,
	spec DifferentialCase,
	requestID uint64,
	upstreamAddress string,
	forwardedPort string,
	connectionClose bool,
) DifferentialObservation {
	t.Helper()
	invocation := differentialDubboProxyInvocation{
		RequestID:       requestID,
		ProtocolVersion: differentialDubboProxyProtocolVersion,
		ServiceName:     differentialDubboProxyServiceName,
		ServiceVersion:  differentialDubboProxyServiceVersion,
		Method:          differentialDubboProxyMethodName,
		ParamsTypeDesc:  differentialDubboProxyParamsTypeDesc,
		HTTPContext: map[string][]string{
			"content-length":    {"12"},
			"extra-arg-k":       {differentialDubboProxyHeaderValue},
			"host":              {differentialDubboProxyHTTPHost},
			"user-agent":        {"Go-http-client/1.1"},
			"x-forwarded-host":  {differentialDubboProxyHTTPHost},
			"x-forwarded-port":  {forwardedPort},
			"x-forwarded-proto": {"http"},
		},
		HTTPBody: differentialDubboProxyRequestBody,
	}
	if connectionClose {
		invocation.HTTPContext["connection"] = []string{"close"}
	}
	encoded, err := json.Marshal(invocation)
	if err != nil {
		t.Fatal(err)
	}
	return DifferentialObservation{
		Status: http.StatusOK,
		Headers: map[string][]string{
			"Got-Extra-Arg-K": {differentialDubboProxyHeaderValue},
		},
		Body: differentialDubboProxyResponseBody, Host: spec.Request.Host,
		SecurityDecision: spec.SecurityDecision,
		UpstreamFixture:  spec.Fixture.Name, UpstreamAddress: upstreamAddress,
		Upstream: DifferentialUpstreamObservation{
			Received: true, Fixture: spec.Fixture.Name,
			Method: differentialDubboProxyWireMethod,
			Path:   differentialDubboProxyServiceName + "/" + differentialDubboProxyMethodName,
			Host:   differentialDubboProxyServiceVersion,
			Headers: map[string][]string{
				differentialDubboProxyParamsTypeHeader: {differentialDubboProxyParamsTypeDesc},
				differentialDubboProxyHTTPHostHeader:   {differentialDubboProxyHTTPHost},
				differentialDubboProxyHTTPBodyHeader:   {differentialDubboProxyRequestBody},
				"Extra-Arg-K":                          {differentialDubboProxyHeaderValue},
			},
			Body: string(encoded),
		},
	}
}
