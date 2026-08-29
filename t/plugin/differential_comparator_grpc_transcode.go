package pluginintegration

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"
)

const differentialGRPCTranscodeUnaryPolicy = "grpc-transcode-unary-h2c"

func init() {
	differentialComparatorRegistry[differentialGRPCTranscodeUnaryPolicy] = differentialComparatorRegistration{
		allowedPlugins: map[string]struct{}{"grpc-transcode": {}},
		compare:        compareDifferentialGRPCTranscodeUnary,
	}
}

func compareDifferentialGRPCTranscodeUnary(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialGRPCTranscodeCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned grpc-transcode case",
			spec.ComparisonPolicy,
		)
	}
	left = copyDifferentialObservation(left)
	right = copyDifferentialObservation(right)
	for _, side := range []struct {
		name        string
		observation *DifferentialObservation
	}{
		{name: "candidate", observation: &left},
		{name: "oracle", observation: &right},
	} {
		if err := normalizeDifferentialGRPCTranscodeObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialGRPCTranscodeObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if len(observation.Steps) != 0 || observation.Status != http.StatusOK ||
		observation.Host != spec.Request.Host || observation.SNI != "" ||
		observation.SecurityDecision != spec.SecurityDecision {
		return fmt.Errorf("%s grpc-transcode gateway response envelope is not exact", side)
	}
	canonicalBody, err := canonicalDifferentialGRPCTranscodeJSON(observation.Body)
	if err != nil {
		return fmt.Errorf("%s grpc-transcode response JSON: %w", side, err)
	}
	observation.Body = canonicalBody
	if err := normalizeDifferentialGRPCContentType(observation.Headers, "application/json"); err != nil {
		return fmt.Errorf("%s grpc-transcode gateway headers: %w", side, err)
	}
	if observation.RetryCount != 0 || len(observation.RouteObserver) != 0 ||
		observation.UpstreamFixture != spec.Fixture.Name || observation.UpstreamAddress == "" {
		return fmt.Errorf("%s grpc-transcode upstream selection is not exact", side)
	}
	upstream := &observation.Upstream
	if !upstream.Received || upstream.Fixture != spec.Fixture.Name ||
		upstream.Method != http.MethodPost || upstream.Path != "/helloworld.Greeter/SayHello" ||
		upstream.Host == "" || len(observation.UpstreamCalls) != 0 {
		return fmt.Errorf("%s grpc-transcode unary upstream envelope is not exact", side)
	}
	if err := normalizeDifferentialGRPCContentType(upstream.Headers, "application/grpc"); err != nil {
		return fmt.Errorf("%s grpc-transcode upstream headers: %w", side, err)
	}
	if err := validateDifferentialUnaryGRPCFrame(
		[]byte(upstream.Body), differentialGRPCRequestMessageBase64,
	); err != nil {
		return fmt.Errorf("%s grpc-transcode upstream frame: %w", side, err)
	}
	upstream.Host = "fixture:" + spec.Fixture.Name
	upstream.Body = "grpc:" + differentialGRPCRequestMessageBase64
	return nil
}

func canonicalDifferentialGRPCTranscodeJSON(body string) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(body))
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil {
		return "", err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return "", fmt.Errorf("response contains multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("response contains trailing non-JSON data: %w", err)
	}
	if len(fields) != 1 || fields["message"] != "Hello world" {
		return "", fmt.Errorf("response fields = %#v, want only Hello world message", fields)
	}
	return `{"message":"Hello world"}`, nil
}

func normalizeDifferentialGRPCContentType(headers map[string][]string, want string) error {
	var values []string
	for name, current := range headers {
		if strings.EqualFold(name, "Content-Type") {
			values = append(values, current...)
			delete(headers, name)
		}
	}
	if len(values) != 1 {
		return fmt.Errorf("Content-Type values = %#v, want one", values)
	}
	mediaType, _, err := mime.ParseMediaType(values[0])
	if err != nil {
		return err
	}
	if !strings.EqualFold(mediaType, want) {
		return fmt.Errorf("Content-Type = %q, want %s", values[0], want)
	}
	headers["Content-Type"] = []string{want}
	return nil
}
