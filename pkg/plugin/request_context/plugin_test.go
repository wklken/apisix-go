package request_context

import "testing"

func TestMetricLabelsDefaultUseIDs(t *testing.T) {
	p := &Plugin{
		config: Config{
			RouteID:     "route-1",
			RouteName:   "route-name",
			ServiceID:   "service-1",
			ServiceName: "service-name",
		},
	}

	labels := p.metricLabels()
	if labels.route != "route-1" {
		t.Fatalf("route label = %q, want route-1", labels.route)
	}
	if labels.service != "service-1" {
		t.Fatalf("service label = %q, want service-1", labels.service)
	}
}

func TestMetricLabelsPreferNameUsesNames(t *testing.T) {
	p := &Plugin{
		config: Config{
			RouteID:              "route-1",
			RouteName:            "route-name",
			ServiceID:            "service-1",
			ServiceName:          "service-name",
			PrometheusPreferName: true,
		},
	}

	labels := p.metricLabels()
	if labels.route != "route-name" {
		t.Fatalf("route label = %q, want route-name", labels.route)
	}
	if labels.service != "service-name" {
		t.Fatalf("service label = %q, want service-name", labels.service)
	}
}

func TestMetricLabelsFallbackToNameWhenIDMissing(t *testing.T) {
	p := &Plugin{
		config: Config{
			RouteName:   "route-name",
			ServiceName: "service-name",
		},
	}

	labels := p.metricLabels()
	if labels.route != "route-name" {
		t.Fatalf("route label = %q, want route-name", labels.route)
	}
	if labels.service != "service-name" {
		t.Fatalf("service label = %q, want service-name", labels.service)
	}
}
