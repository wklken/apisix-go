package pluginintegration

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCompareDifferentialKafkaLoggerProduceValidatesRecordSemantics(t *testing.T) {
	spec := differentialCasesForPlugin("kafka-logger")[0]
	candidate := differentialKafkaLoggerObservation(
		t,
		spec,
		"candidate",
		"127.0.0.1:19092",
		"GET /hello?ab=cd HTTP/1.1\r\nHost: localhost\r\nContent-Length: 6\r\nUser-Agent: Go-http-client/1.1\r\nX-Forwarded-Host: localhost\r\nX-Forwarded-Port: 31000\r\nX-Forwarded-Proto: http\r\n\r\nabcdef",
	)
	oracle := differentialKafkaLoggerObservation(
		t,
		spec,
		"oracle",
		"host.containers.internal:19092",
		"GET /hello?ab=cd HTTP/1.1\r\nConnection: close\r\nUser-Agent: Go-http-client/1.1\r\nContent-Length: 6\r\nHost: localhost\r\nX-Forwarded-Host: localhost\r\nX-Forwarded-Port: 9080\r\nX-Forwarded-Proto: http\r\n\r\nabcdef",
	)
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)
	passed, detail, err := compareDifferentialKafkaLoggerProduce(
		spec,
		candidate,
		oracle,
		testNormalizationPolicy(),
	)
	if err != nil || !passed || detail != "" {
		t.Fatalf("compare = (%v, %q, %v), want pass", passed, detail, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("comparator mutated its inputs")
	}
}

func TestCompareDifferentialKafkaLoggerProduceRejectsWeakEvidence(t *testing.T) {
	spec := differentialCasesForPlugin("kafka-logger")[0]
	base := differentialKafkaLoggerObservation(
		t,
		spec,
		"candidate",
		"127.0.0.1:19092",
		"GET /hello?ab=cd HTTP/1.1\r\nHost: localhost\r\nContent-Length: 6\r\n\r\nabcdef",
	)
	for _, test := range []struct {
		name   string
		mutate func(*DifferentialObservation)
	}{
		{name: "wrong topic", mutate: func(observation *DifferentialObservation) {
			observation.UpstreamCalls[1].Path = "other"
		}},
		{name: "wrong key", mutate: func(observation *DifferentialObservation) {
			observation.UpstreamCalls[1].Host = "other"
		}},
		{name: "missing body", mutate: func(observation *DifferentialObservation) {
			observation.UpstreamCalls[1].Body = "GET /hello?ab=cd HTTP/1.1\r\nHost: localhost\r\nContent-Length: 6\r\n\r\n"
		}},
		{name: "extra Kafka record", mutate: func(observation *DifferentialObservation) {
			observation.UpstreamCalls = append(observation.UpstreamCalls, observation.UpstreamCalls[1])
		}},
		{name: "invalid forwarded port", mutate: func(observation *DifferentialObservation) {
			observation.UpstreamCalls[1].Body = strings.Replace(
				observation.UpstreamCalls[1].Body,
				"Content-Length: 6\r\n",
				"Content-Length: 6\r\nX-Forwarded-Port: invalid\r\n",
				1,
			)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := copyDifferentialObservation(base)
			test.mutate(&candidate)
			passed, _, err := compareDifferentialKafkaLoggerProduce(
				spec,
				candidate,
				base,
				testNormalizationPolicy(),
			)
			if err == nil || passed {
				t.Fatalf("compare = (%v, %v), want evidence rejection", passed, err)
			}
		})
	}
}

func TestCompareDifferentialKafkaLoggerProduceRejectsAlteredCase(t *testing.T) {
	spec := differentialCasesForPlugin("kafka-logger")[0]
	spec.Config["extra"] = true
	_, _, err := compareDifferentialKafkaLoggerProduce(
		spec,
		DifferentialObservation{},
		DifferentialObservation{},
		NormalizationPolicy{},
	)
	if err == nil {
		t.Fatal("altered case was accepted")
	}
}

func differentialKafkaLoggerObservation(
	t *testing.T,
	spec DifferentialCase,
	side string,
	address string,
	record string,
) DifferentialObservation {
	t.Helper()
	headers := map[string][]string{
		"Content-Length": {"12"},
		"Content-Type":   {"text/plain; charset=utf-8"},
		"Server":         {"APISIX/3.17.0"},
	}
	if side == "candidate" {
		headers["Server"] = []string{"APISIX/dev"}
		headers["Date"] = []string{time.Now().UTC().Format(http.TimeFormat)}
	}
	origin := DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name,
		Method: spec.Request.Method, Path: spec.Request.Path,
		Host: "differential.example.test", Body: spec.Request.Body,
	}
	kafkaRecord := DifferentialUpstreamObservation{
		Received: true, Fixture: spec.Fixture.Name,
		Method: differentialKafkaMethod, Path: "test2", Host: "key1", Body: record,
	}
	return DifferentialObservation{
		Status: spec.Fixture.Response.Status, Headers: headers, Body: spec.Fixture.Response.Body,
		Host: spec.Request.Host, SecurityDecision: spec.SecurityDecision,
		UpstreamFixture: spec.Fixture.Name, UpstreamAddress: address,
		Upstream: kafkaRecord, UpstreamCalls: []DifferentialUpstreamObservation{origin, kafkaRecord},
	}
}
