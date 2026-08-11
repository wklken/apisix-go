package base

import "testing"

type phaseDescriptorTestConfig struct {
	stage  string
	header bool
	body   bool
}

func (c phaseDescriptorTestConfig) DescribeBindingPhases() (BindingPhaseDescriptor, error) {
	return BindingPhaseDescriptor{RequestStage: c.stage, Header: c.header, BufferedBody: c.body}, nil
}

func TestBindingPhaseDescriptorIsConfigOnly(t *testing.T) {
	descriptor, err := (phaseDescriptorTestConfig{stage: "none", header: true}).DescribeBindingPhases()
	if err != nil || descriptor.RequestStage != "none" || !descriptor.Header || descriptor.BufferedBody {
		t.Fatalf("descriptor = %#v, err=%v", descriptor, err)
	}
}
