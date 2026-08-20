package base

import "testing"

type phaseDescriptorTestConfig struct {
	stage           string
	header          bool
	streamingHeader bool
	body            bool
}

func (c phaseDescriptorTestConfig) DescribeBindingPhases() (BindingPhaseDescriptor, error) {
	return BindingPhaseDescriptor{
		RequestStage: c.stage, Header: c.header, StreamingHeader: c.streamingHeader, BufferedBody: c.body,
	}, nil
}

func TestBindingPhaseDescriptorIsConfigOnly(t *testing.T) {
	descriptor, err := (phaseDescriptorTestConfig{stage: "none", header: true}).DescribeBindingPhases()
	if err != nil || descriptor.RequestStage != "none" || !descriptor.Header || descriptor.BufferedBody {
		t.Fatalf("descriptor = %#v, err=%v", descriptor, err)
	}
}

func TestBindingPhaseDescriptorDistinguishesStreamingHeader(t *testing.T) {
	descriptor, err := (phaseDescriptorTestConfig{stage: "none", streamingHeader: true}).DescribeBindingPhases()
	if err != nil || descriptor.RequestStage != "none" || descriptor.Header || !descriptor.StreamingHeader {
		t.Fatalf("descriptor = %#v, err=%v", descriptor, err)
	}
}
