package pluginintegration

import (
	"fmt"
	"net/http"
	"reflect"
)

const differentialHTTPDubboPOJOPolicy = "http-dubbo-pojo-fastjson"

func init() {
	differentialComparatorRegistry[differentialHTTPDubboPOJOPolicy] = differentialComparatorRegistration{
		allowedPlugins: map[string]struct{}{"http-dubbo": {}},
		compare:        compareDifferentialHTTPDubboPOJO,
	}
}

func compareDifferentialHTTPDubboPOJO(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, mustDifferentialCase(spec.Name)) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned http-dubbo case",
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
		if err := normalizeDifferentialHTTPDubboObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialHTTPDubboObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if len(observation.Steps) != 0 || observation.Status != http.StatusOK ||
		observation.Body != differentialHTTPDubboPOJOJSON || observation.Host != spec.Request.Host ||
		observation.SNI != "" || observation.SecurityDecision != spec.SecurityDecision ||
		observation.RetryCount != 0 || len(observation.RouteObserver) != 0 || observation.File != nil {
		return fmt.Errorf("%s http-dubbo gateway response envelope is not exact", side)
	}
	if observation.UpstreamFixture != spec.Fixture.Name || observation.UpstreamAddress == "" ||
		len(observation.UpstreamCalls) != 0 {
		return fmt.Errorf("%s http-dubbo upstream selection is not exact", side)
	}
	upstream := &observation.Upstream
	if !upstream.Received || upstream.Fixture != spec.Fixture.Name ||
		upstream.Method != differentialHTTPDubboMethod ||
		upstream.Path != differentialHTTPDubboServiceName+"/"+differentialHTTPDubboMethodName ||
		upstream.Host != differentialHTTPDubboServiceVersion ||
		!reflect.DeepEqual(upstream.Headers, map[string][]string{
			differentialHTTPDubboParamsTypeHeader: {differentialHTTPDubboParamsTypeDesc},
		}) {
		return fmt.Errorf("%s http-dubbo upstream envelope is not exact", side)
	}
	if err := validateDifferentialHTTPDubboRequestFrame([]byte(upstream.Body)); err != nil {
		return fmt.Errorf("%s http-dubbo request frame: %w", side, err)
	}
	upstream.Body = "dubbo-fastjson:" + differentialHTTPDubboRequestFrameBase64
	return nil
}
