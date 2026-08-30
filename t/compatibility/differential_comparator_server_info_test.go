package pluginintegration

import "testing"

func TestDifferentialServerInfoControlAPIPolicyValidatesDynamicFieldsPerSide(t *testing.T) {
	spec := differentialCasesForPlugin("server-info")[0]
	candidate := differentialServerInfoObservation(
		`{"boot_time":1787940000,"etcd_version":"unknown","hostname":"candidate.local","id":"candidate-1","version":"apisix-go"}`,
		"application/json; charset=UTF-8",
	)
	oracle := differentialServerInfoObservation(
		`{"boot_time":1787939990,"etcd_version":"3.6.4","hostname":"oracle-abc","id":"123456","version":"3.17.0"}`,
		"application/json",
	)

	equal, detail, err := compareDifferentialServerInfoControlAPI(
		spec,
		candidate,
		oracle,
		testNormalizationPolicy(),
	)
	if err != nil || !equal {
		t.Fatalf("compare server-info = %t, %q, %v", equal, detail, err)
	}
}

func TestDifferentialServerInfoControlAPIPolicyRejectsMalformedSemantics(t *testing.T) {
	spec := differentialCasesForPlugin("server-info")[0]
	valid := differentialServerInfoObservation(
		`{"boot_time":1787940000,"etcd_version":"unknown","hostname":"candidate.local","id":"candidate-1","version":"apisix-go"}`,
		"application/json",
	)
	tests := []struct {
		name string
		body string
	}{
		{
			name: "zero boot time",
			body: `{"boot_time":0,"etcd_version":"unknown","hostname":"candidate.local","id":"candidate-1","version":"apisix-go"}`,
		},
		{
			name: "extra field",
			body: `{"boot_time":1787940000,"etcd_version":"unknown","hostname":"candidate.local","id":"candidate-1","version":"apisix-go","extra":true}`,
		},
		{
			name: "invalid hostname",
			body: `{"boot_time":1787940000,"etcd_version":"unknown","hostname":"bad host","id":"candidate-1","version":"apisix-go"}`,
		},
		{
			name: "invalid version",
			body: `{"boot_time":1787940000,"etcd_version":"unknown","hostname":"candidate.local","id":"candidate-1","version":""}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			malformed := differentialServerInfoObservation(test.body, "application/json")
			if equal, _, err := compareDifferentialServerInfoControlAPI(
				spec,
				malformed,
				valid,
				testNormalizationPolicy(),
			); err == nil || equal {
				t.Fatalf("compare malformed server-info = %t/%v, want false/error", equal, err)
			}
		})
	}
}

func differentialServerInfoObservation(body, contentType string) DifferentialObservation {
	return DifferentialObservation{
		Status: 200,
		Headers: map[string][]string{
			"Content-Type": {contentType},
		},
		Body:             body,
		Host:             "gateway.example.test",
		SecurityDecision: "not_applicable",
	}
}
