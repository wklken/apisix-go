package pluginintegration

import (
	"fmt"
	"mime"
	"net/http"
	"reflect"
	"strings"
)

func init() {
	differentialComparatorRegistry[differentialProxyBufferingSSEPolicy] = differentialComparatorRegistration{
		allowedPlugins: map[string]struct{}{"proxy-buffering": {}},
		compare:        compareDifferentialProxyBufferingSSEEnvelope,
	}
}

func compareDifferentialProxyBufferingSSEEnvelope(
	spec DifferentialCase,
	candidate DifferentialObservation,
	oracle DifferentialObservation,
	_ NormalizationPolicy,
) (bool, string, error) {
	streamCase, err := differentialProxyBufferingStreamCaseForSpec(spec)
	if err != nil {
		return false, "", err
	}
	candidateStream, err := parseDifferentialProxyBufferingSSEEnvelope(spec, "candidate", candidate)
	if err != nil {
		return false, "", err
	}
	oracleStream, err := parseDifferentialProxyBufferingSSEEnvelope(spec, "oracle", oracle)
	if err != nil {
		return false, "", err
	}
	return compareDifferentialProxyBufferingSSE(streamCase, candidateStream, oracleStream)
}

func parseDifferentialProxyBufferingSSEEnvelope(
	spec DifferentialCase,
	side string,
	observation DifferentialObservation,
) (differentialSSEStreamObservation, error) {
	wantHeaders := map[string][]string{"Content-Type": {"text/event-stream"}}
	if observation.Status != http.StatusOK || !reflect.DeepEqual(observation.Headers, wantHeaders) ||
		observation.Host != spec.Request.Host || observation.SNI != "" ||
		observation.SecurityDecision != spec.SecurityDecision || len(observation.Steps) != 0 ||
		len(observation.RouteObserver) != 0 || observation.UpstreamFixture != "" ||
		observation.UpstreamAddress != "" || observation.RetryCount != 0 ||
		observation.Upstream.Received || len(observation.UpstreamCalls) != 0 || observation.File != nil {
		return differentialSSEStreamObservation{}, fmt.Errorf(
			"%s proxy-buffering SSE observation envelope is not exact",
			side,
		)
	}
	stream, err := parseDifferentialSSEDriverOutput([]byte(observation.Body))
	if err != nil {
		return differentialSSEStreamObservation{}, fmt.Errorf("%s proxy-buffering SSE transcript: %w", side, err)
	}
	return stream, nil
}

func compareDifferentialProxyBufferingSSE(
	streamCase differentialProxyBufferingStreamCase,
	candidate differentialSSEStreamObservation,
	oracle differentialSSEStreamObservation,
) (bool, string, error) {
	if !reflect.DeepEqual(streamCase, differentialProxyBufferingStreamingCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned proxy-buffering SSE case",
			streamCase.Spec.ComparisonPolicy,
		)
	}
	if err := validateDifferentialProxyBufferingSSEObservation(streamCase, "candidate", candidate); err != nil {
		return false, "", err
	}
	if err := validateDifferentialProxyBufferingSSEObservation(streamCase, "oracle", oracle); err != nil {
		return false, "", err
	}
	if !reflect.DeepEqual(candidate, oracle) {
		return false, "incremental SSE observations differ", nil
	}
	return true, "", nil
}

func validateDifferentialProxyBufferingSSEObservation(
	streamCase differentialProxyBufferingStreamCase,
	side string,
	observation differentialSSEStreamObservation,
) error {
	if observation.Status != http.StatusOK {
		return fmt.Errorf("%s SSE status = %d, want 200", side, observation.Status)
	}
	mediaType, _, err := mime.ParseMediaType(observation.ContentType)
	if err != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		return fmt.Errorf("%s SSE Content-Type = %q, want text/event-stream", side, observation.ContentType)
	}
	if !reflect.DeepEqual(observation.Frames, streamCase.Contract.Frames[:streamCase.Contract.RequiredFrames]) {
		return fmt.Errorf(
			"%s SSE frames = %#v, want exact ordered frames %#v",
			side,
			observation.Frames,
			streamCase.Contract.Frames[:streamCase.Contract.RequiredFrames],
		)
	}
	if !observation.ConnectionOpenAfterRequiredFrames {
		return fmt.Errorf(
			"%s SSE connection reached EOF after the required frames; incremental arrival before EOF is not proven",
			side,
		)
	}
	return nil
}
