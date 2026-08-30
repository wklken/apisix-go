package pluginintegration

import (
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func differentialNetworkLoggerCandidateGatewayHeaders(bodyLength int, contentType string) map[string][]string {
	return map[string][]string{
		"Content-Length": {strconv.Itoa(bodyLength)},
		"Content-Type":   {contentType},
		"Date":           {"Fri, 28 Aug 2026 17:24:08 GMT"},
		"Server":         {"APISIX/test-candidate"},
	}
}

func differentialNetworkLoggerOracleGatewayHeaders(bodyLength int, contentType string) map[string][]string {
	return map[string][]string{
		"Content-Length": {strconv.Itoa(bodyLength)},
		"Content-Type":   {contentType},
		"Server":         {"APISIX/3.17.0"},
	}
}

func TestCompareDifferentialLoggerFixtureDeliveryAcceptsUnorderedSemanticCalls(t *testing.T) {
	for _, test := range []struct {
		name    string
		spec    DifferentialCase
		compare differentialComparator
		pair    func(DifferentialCase) (DifferentialObservation, DifferentialObservation)
	}{
		{
			name: "clickhouse",
			spec: differentialCasesForPlugin("clickhouse-logger")[0], compare: compareDifferentialClickHouseLoggerFixtureDelivery,
			pair: differentialClickHouseLoggerComparatorObservations,
		},
		{
			name: "elasticsearch",
			spec: differentialCasesForPlugin("elasticsearch-logger")[0], compare: compareDifferentialElasticsearchLoggerFixtureDelivery,
			pair: differentialElasticsearchLoggerComparatorObservations,
		},
		{
			name: "http",
			spec: differentialCasesForPlugin("http-logger")[0], compare: compareDifferentialHTTPLoggerFixtureDelivery,
			pair: differentialHTTPLoggerComparatorObservations,
		},
		{
			name: "loki",
			spec: differentialCasesForPlugin("loki-logger")[0], compare: compareDifferentialLokiLoggerFixtureDelivery,
			pair: differentialLokiLoggerComparatorObservations,
		},
		{
			name: "splunk",
			spec: differentialCasesForPlugin("splunk-hec-logging")[0], compare: compareDifferentialSplunkHECLoggingFixtureDelivery,
			pair: differentialSplunkLoggerComparatorObservations,
		},
		{
			name: "tencent-cls",
			spec: differentialCasesForPlugin("tencent-cloud-cls")[0], compare: compareDifferentialTencentCloudCLSFixtureDelivery,
			pair: differentialTencentCLSLoggerComparatorObservations,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, oracle := test.pair(test.spec)
			candidateBefore := copyDifferentialObservation(candidate)
			oracleBefore := copyDifferentialObservation(oracle)

			passed, diff, err := test.compare(
				test.spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err != nil || !passed || diff != "" {
				t.Fatalf("compare pinned logger observations = %t, %q, %v", passed, diff, err)
			}
			if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
				t.Fatal("logger comparison mutated caller observations")
			}
		})
	}
}

func TestCompareDifferentialLoggerFixtureDeliveryTreatsEmptyQueryMarkerAsEquivalent(t *testing.T) {
	for _, test := range []struct {
		name    string
		spec    DifferentialCase
		compare differentialComparator
		pair    func(DifferentialCase) (DifferentialObservation, DifferentialObservation)
		path    string
	}{
		{
			name: "clickhouse", spec: differentialCasesForPlugin("clickhouse-logger")[0],
			compare: compareDifferentialClickHouseLoggerFixtureDelivery,
			pair:    differentialClickHouseLoggerComparatorObservations, path: "/clickhouse",
		},
		{
			name: "http", spec: differentialCasesForPlugin("http-logger")[0],
			compare: compareDifferentialHTTPLoggerFixtureDelivery,
			pair:    differentialHTTPLoggerComparatorObservations, path: "/http-log",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, oracle := test.pair(test.spec)
			differentialLoggerTestCall(&oracle, test.path).Path += "?"

			passed, diff, err := test.compare(
				test.spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err != nil || !passed || diff != "" {
				t.Fatalf("compare equivalent empty query marker = %t, %q, %v", passed, diff, err)
			}
		})
	}
}

func TestDecodeDifferentialTencentCLSContentAcceptsProtobufFieldOrder(t *testing.T) {
	body := protowire.AppendTag(nil, 2, protowire.BytesType)
	body = protowire.AppendBytes(body, []byte("tencent-cloud-cls"))
	body = protowire.AppendTag(body, 1, protowire.BytesType)
	body = protowire.AppendBytes(body, []byte("case"))

	key, value, err := decodeDifferentialTencentCLSContent(body)
	if err != nil {
		t.Fatalf("decode APISIX 3.17 value-first CLS content: %v", err)
	}
	if key != "case" || value != "tencent-cloud-cls" {
		t.Fatalf("decoded CLS content = %q/%q, want case/tencent-cloud-cls", key, value)
	}
}

func TestCompareDifferentialLoggerFixtureDeliveryRejectsAdditionalQueryParameters(t *testing.T) {
	spec := differentialCasesForPlugin("elasticsearch-logger")[0]
	candidate, oracle := differentialElasticsearchLoggerComparatorObservations(spec)
	call := differentialLoggerTestCall(&candidate, "/_bulk")
	call.Path = "/_bulk?timeout=10000ms"
	candidate.Upstream = *call

	passed, _, err := compareDifferentialElasticsearchLoggerFixtureDelivery(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err == nil || passed || !strings.Contains(err.Error(), "missing POST /_bulk") {
		t.Fatalf("compare extra query parameter = %t, %v, want strict path rejection", passed, err)
	}
}

func TestCompareDifferentialLoggerFixtureDeliveryRejectsLooseContracts(t *testing.T) {
	for _, test := range []struct {
		name    string
		spec    DifferentialCase
		compare differentialComparator
		pair    func(DifferentialCase) (DifferentialObservation, DifferentialObservation)
		mutate  func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation)
		want    string
	}{
		{
			name: "unpinned case", spec: differentialCasesForPlugin("http-logger")[0],
			compare: compareDifferentialHTTPLoggerFixtureDelivery,
			pair:    differentialHTTPLoggerComparatorObservations,
			mutate: func(spec *DifferentialCase, _, _ *DifferentialObservation) {
				spec.Fixture.ExpectedCalls++
			},
			want: "pinned",
		},
		{
			name: "wrong gateway response", spec: differentialCasesForPlugin("clickhouse-logger")[0],
			compare: compareDifferentialClickHouseLoggerFixtureDelivery,
			pair:    differentialClickHouseLoggerComparatorObservations,
			mutate: func(_ *DifferentialCase, candidate, oracle *DifferentialObservation) {
				candidate.Steps[0].Status = http.StatusCreated
				oracle.Steps[0].Status = http.StatusCreated
			},
			want: "gateway step",
		},
		{
			name: "missing origin", spec: differentialCasesForPlugin("loki-logger")[0],
			compare: compareDifferentialLokiLoggerFixtureDelivery,
			pair:    differentialLokiLoggerComparatorObservations,
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls = candidate.UpstreamCalls[1:]
			},
			want: "fixture calls",
		},
		{
			name: "wrong origin host", spec: differentialCasesForPlugin("http-logger")[0],
			compare: compareDifferentialHTTPLoggerFixtureDelivery,
			pair:    differentialHTTPLoggerComparatorObservations,
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				differentialLoggerTestCall(oracle, "/logger/http").Host = oracle.UpstreamAddress
				oracle.Upstream.Host = oracle.UpstreamAddress
			},
			want: "origin request",
		},
		{
			name: "clickhouse missing credential header", spec: differentialCasesForPlugin("clickhouse-logger")[0],
			compare: compareDifferentialClickHouseLoggerFixtureDelivery,
			pair:    differentialClickHouseLoggerComparatorObservations,
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				deleteDifferentialHeader(candidate.UpstreamCalls[1].Headers, "X-ClickHouse-Key")
			},
			want: "X-ClickHouse-Key",
		},
		{
			name: "clickhouse malformed JSON row", spec: differentialCasesForPlugin("clickhouse-logger")[0],
			compare: compareDifferentialClickHouseLoggerFixtureDelivery,
			pair:    differentialClickHouseLoggerComparatorObservations,
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				differentialLoggerTestCall(oracle, "/clickhouse").Body = `INSERT INTO logs FORMAT JSONEachRow {"case":"wrong"}`
			},
			want: "ClickHouse payload",
		},
		{
			name: "elasticsearch extra NDJSON line", spec: differentialCasesForPlugin("elasticsearch-logger")[0],
			compare: compareDifferentialElasticsearchLoggerFixtureDelivery,
			pair:    differentialElasticsearchLoggerComparatorObservations,
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls[2].Body += `{}` + "\n"
				candidate.Upstream.Body = candidate.UpstreamCalls[2].Body
			},
			want: "Elasticsearch NDJSON",
		},
		{
			name: "http wrong auth", spec: differentialCasesForPlugin("http-logger")[0],
			compare: compareDifferentialHTTPLoggerFixtureDelivery,
			pair:    differentialHTTPLoggerComparatorObservations,
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				differentialLoggerTestCall(oracle, "/http-log").Headers["Authorization"] = []string{"Basic other"}
			},
			want: "Authorization",
		},
		{
			name: "loki missing tenant", spec: differentialCasesForPlugin("loki-logger")[0],
			compare: compareDifferentialLokiLoggerFixtureDelivery,
			pair:    differentialLokiLoggerComparatorObservations,
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				deleteDifferentialHeader(candidate.UpstreamCalls[1].Headers, "X-Scope-OrgID")
			},
			want: "X-Scope-OrgID",
		},
		{
			name: "loki bad timestamp", spec: differentialCasesForPlugin("loki-logger")[0],
			compare: compareDifferentialLokiLoggerFixtureDelivery,
			pair:    differentialLokiLoggerComparatorObservations,
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialLoggerTestCall(oracle, "/loki/api/v1/push")
				call.Body = strings.Replace(
					call.Body, `"1700000000000000000"`, `"01700000000000000000"`, 1,
				)
			},
			want: "Loki payload",
		},
		{
			name: "splunk wrong token", spec: differentialCasesForPlugin("splunk-hec-logging")[0],
			compare: compareDifferentialSplunkHECLoggingFixtureDelivery,
			pair:    differentialSplunkLoggerComparatorObservations,
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls[1].Headers["Authorization"] = []string{"Splunk wrong"}
			},
			want: "Authorization",
		},
		{
			name: "splunk wrong event", spec: differentialCasesForPlugin("splunk-hec-logging")[0],
			compare: compareDifferentialSplunkHECLoggingFixtureDelivery,
			pair:    differentialSplunkLoggerComparatorObservations,
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialLoggerTestCall(oracle, "/services/collector")
				call.Body = strings.Replace(
					call.Body, "differential-splunk-event", "wrong", 1,
				)
			},
			want: "Splunk payload",
		},
		{
			name: "cls malformed protobuf", spec: differentialCasesForPlugin("tencent-cloud-cls")[0],
			compare: compareDifferentialTencentCloudCLSFixtureDelivery,
			pair:    differentialTencentCLSLoggerComparatorObservations,
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls[1].Body = "not-protobuf"
				candidate.Upstream.Body = "not-protobuf"
			},
			want: "CLS protobuf",
		},
		{
			name: "cls invalid signature", spec: differentialCasesForPlugin("tencent-cloud-cls")[0],
			compare: compareDifferentialTencentCloudCLSFixtureDelivery,
			pair:    differentialTencentCLSLoggerComparatorObservations,
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := differentialLoggerTestCall(
					oracle, "/structuredlog?topic_id=143b5d70-139b-4aec-b54e-bb97756916de",
				)
				authorization := call.Headers["Authorization"][0]
				call.Headers["Authorization"] = []string{
					strings.TrimSuffix(authorization, "18") + "19",
				}
			},
			want: "CLS Authorization",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, oracle := test.pair(test.spec)
			test.mutate(&test.spec, &candidate, &oracle)
			passed, _, err := test.compare(test.spec, candidate, oracle, testNormalizationPolicy())
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare malformed logger contract = %t, %v, want %q", passed, err, test.want)
			}
		})
	}
}

func TestCompareDifferentialLoggerFixtureDeliveryKeepsNonvolatileFieldsStrict(t *testing.T) {
	spec := differentialCasesForPlugin("http-logger")[0]
	candidate, oracle := differentialHTTPLoggerComparatorObservations(spec)
	oracle.Steps[0].Headers = map[string][]string{"X-Plugin-Result": {"changed"}}

	passed, _, err := compareDifferentialHTTPLoggerFixtureDelivery(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil {
		t.Fatalf("compare strict response header: %v", err)
	}
	if passed {
		t.Fatal("nonvolatile response header difference was normalized")
	}
}

func differentialClickHouseLoggerComparatorObservations(
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	loggerCall := DifferentialUpstreamObservation{
		Received: true, Fixture: "origin-and-clickhouse", Method: http.MethodPost,
		Path: "/clickhouse", Host: "127.0.0.1:31001",
		Headers: map[string][]string{
			"Content-Type":          {"application/json"},
			"X-ClickHouse-User":     {"default"},
			"X-ClickHouse-Key":      {"differential-password"},
			"X-ClickHouse-Database": {"default"},
		},
		Body: `INSERT INTO logs FORMAT JSONEachRow {"case":"clickhouse-logger","route_id":"` + spec.RouteID + `"}`,
	}
	candidate, oracle := differentialLoggerComparatorPair(
		spec, "127.0.0.1:31001", "host.containers.internal:1980", loggerCall,
	)
	differentialLoggerTestCall(&oracle, "/clickhouse").Host = "host.containers.internal"
	return candidate, oracle
}

func differentialElasticsearchLoggerComparatorObservations(
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	version := DifferentialUpstreamObservation{
		Received: true, Fixture: "version-origin-and-bulk", Method: http.MethodGet,
		Path: "/", Host: "127.0.0.1:31002",
	}
	bulk := DifferentialUpstreamObservation{
		Received: true, Fixture: "version-origin-and-bulk", Method: http.MethodPost,
		Path: "/_bulk", Host: "127.0.0.1:31002",
		Headers: map[string][]string{"Content-Type": {"application/x-ndjson"}},
		Body: "{\"index\":{\"_index\":\"services\"}}\n" +
			"{\"custom_case\":\"elasticsearch-logger\",\"route_id\":\"" + spec.RouteID + "\"}\n",
	}
	candidate, oracle := differentialLoggerComparatorPair(
		spec, "127.0.0.1:31002", "host.containers.internal:1980", version, bulk,
	)
	return candidate, oracle
}

func differentialHTTPLoggerComparatorObservations(
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	loggerCall := DifferentialUpstreamObservation{
		Received: true, Fixture: "origin-and-http-log", Method: http.MethodPost,
		Path: "/http-log", Host: "127.0.0.1:31003",
		Headers: map[string][]string{
			"Authorization": {"Basic differential"},
			"Content-Type":  {"application/json"},
		},
		Body: `{"case":"http-logger","route_id":"` + spec.RouteID + `"}`,
	}
	candidate, oracle := differentialLoggerComparatorPair(
		spec, "127.0.0.1:31003", "host.containers.internal:1980", loggerCall,
	)
	differentialLoggerTestCall(&oracle, "/http-log").Host = "host.containers.internal"
	return candidate, oracle
}

func differentialLokiLoggerComparatorObservations(
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	loggerCall := DifferentialUpstreamObservation{
		Received: true,
		Fixture:  "origin-and-loki",
		Method:   http.MethodPost,
		Path:     "/loki/api/v1/push",
		Host:     "127.0.0.1:31004",
		Headers: map[string][]string{
			"Authorization": {"test1234"},
			"Content-Type":  {"application/json"},
			"X-Scope-OrgID": {"tenant-differential"},
		},
		Body: `{"streams":[{"stream":{"job":"apisix-differential"},"values":[["1700000000000000000","{\"case\":\"loki-logger\",\"route_id\":\"` + spec.RouteID + `\"}"]]}]}`,
	}
	return differentialLoggerComparatorPair(spec, "127.0.0.1:31004", "host.containers.internal:1980", loggerCall)
}

func differentialSplunkLoggerComparatorObservations(
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	loggerCall := DifferentialUpstreamObservation{
		Received: true,
		Fixture:  "origin-and-splunk",
		Method:   http.MethodPost,
		Path:     "/services/collector",
		Host:     "127.0.0.1:31005",
		Headers: map[string][]string{
			"Authorization": {"Splunk BD274822-96AA-4DA6-90EC-18940FB2414C"},
			"Content-Type":  {"application/json"},
		},
		Body: `{"time":1700000000.25,"host":"candidate-host","source":"apache-apisix-splunk-hec-logging","sourcetype":"_json","event":{"message":"differential-splunk-event","route_id":"` + spec.RouteID + `"}}`,
	}
	candidate, oracle := differentialLoggerComparatorPair(
		spec, "127.0.0.1:31005", "host.containers.internal:1980", loggerCall,
	)
	oracleCall := differentialLoggerTestCall(&oracle, "/services/collector")
	oracleCall.Body = strings.Replace(oracleCall.Body, "candidate-host", "oracle-host", 1)
	oracle.Upstream = oracle.UpstreamCalls[len(oracle.UpstreamCalls)-1]
	return candidate, oracle
}

func differentialTencentCLSLoggerComparatorObservations(
	spec DifferentialCase,
) (DifferentialObservation, DifferentialObservation) {
	payload := differentialTencentCLSComparatorPayload(spec.RouteID)
	loggerCall := DifferentialUpstreamObservation{
		Received: true, Fixture: "origin-and-tencent-cls", Method: http.MethodPost,
		Path: "/structuredlog?topic_id=143b5d70-139b-4aec-b54e-bb97756916de",
		Host: "127.0.0.1:31006",
		Headers: map[string][]string{
			"Authorization": {
				"q-sign-algorithm=sha1&q-ak=secret_id&q-sign-time=1700000000;1700000060&q-key-time=1700000000;1700000060&q-header-list=&q-url-param-list=&q-signature=f6856fc76515a382fa6f8f38a9d3d1d395515f18",
			},
			"Content-Type": {"application/x-protobuf"},
		},
		Body: string(payload),
	}
	return differentialLoggerComparatorPair(spec, "127.0.0.1:31006", "host.containers.internal:1980", loggerCall)
}

func differentialLoggerComparatorPair(
	spec DifferentialCase,
	candidateAddress string,
	oracleAddress string,
	additionalCalls ...DifferentialUpstreamObservation,
) (DifferentialObservation, DifferentialObservation) {
	origin := DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name, Method: http.MethodGet,
		Path: spec.Steps[0].Request.Path, Host: "differential.example.test",
	}
	candidateCalls := append([]DifferentialUpstreamObservation{origin}, additionalCalls...)
	for index := 1; index < len(candidateCalls); index++ {
		candidateCalls[index].Host = candidateAddress
	}
	candidate := DifferentialObservation{
		Steps: []DifferentialStepObservation{{
			Status: spec.Fixture.Response.Status,
			Headers: func() map[string][]string {
				if contentType := spec.Fixture.Response.Headers["Content-Type"]; contentType != "" {
					return map[string][]string{"Content-Type": {contentType}}
				}
				return nil
			}(),
			Body: spec.Fixture.Response.Body, Host: spec.Steps[0].Request.Host,
			SecurityDecision: spec.Steps[0].SecurityDecision,
		}},
		UpstreamFixture: spec.Fixture.Name, UpstreamAddress: candidateAddress,
		UpstreamCalls: candidateCalls, Upstream: candidateCalls[len(candidateCalls)-1],
	}
	oracle := copyDifferentialObservation(candidate)
	oracle.UpstreamAddress = oracleAddress
	for index := 1; index < len(oracle.UpstreamCalls); index++ {
		oracle.UpstreamCalls[index].Host = oracleAddress
	}
	oracle.Upstream = oracle.UpstreamCalls[len(oracle.UpstreamCalls)-1]
	if len(oracle.UpstreamCalls) > 1 {
		oracle.UpstreamCalls[0], oracle.UpstreamCalls[len(oracle.UpstreamCalls)-1] = oracle.UpstreamCalls[len(oracle.UpstreamCalls)-1], oracle.UpstreamCalls[0]
		oracle.Upstream = oracle.UpstreamCalls[len(oracle.UpstreamCalls)-1]
	}
	return candidate, oracle
}

func differentialTencentCLSComparatorPayload(routeID string) []byte {
	appendContent := func(body []byte, key string, value string) []byte {
		content := protowire.AppendTag(nil, 1, protowire.BytesType)
		content = protowire.AppendBytes(content, []byte(key))
		content = protowire.AppendTag(content, 2, protowire.BytesType)
		content = protowire.AppendBytes(content, []byte(value))
		body = protowire.AppendTag(body, 2, protowire.BytesType)
		return protowire.AppendBytes(body, content)
	}
	logEntry := protowire.AppendTag(nil, 1, protowire.VarintType)
	logEntry = protowire.AppendVarint(logEntry, 1700000000000)
	logEntry = appendContent(logEntry, "case", "tencent-cloud-cls")
	logEntry = appendContent(logEntry, "route_id", routeID)
	group := protowire.AppendTag(nil, 1, protowire.BytesType)
	group = protowire.AppendBytes(group, logEntry)
	group = protowire.AppendTag(group, 4, protowire.BytesType)
	group = protowire.AppendBytes(group, []byte("192.0.2.10"))
	list := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(list, group)
}

func differentialLoggerTestCall(
	observation *DifferentialObservation,
	path string,
) *DifferentialUpstreamObservation {
	for index := range observation.UpstreamCalls {
		if observation.UpstreamCalls[index].Path == path {
			return &observation.UpstreamCalls[index]
		}
	}
	panic("missing differential logger test call " + path)
}
