package base

// BindingPhaseDescriptor is the config-derived part of a checked plugin
// binding. The root plugin package validates the exact request-stage strings
// and response capability mask for each factory identity.
type BindingPhaseDescriptor struct {
	RequestStage    string
	Header          bool
	StreamingHeader bool
	BufferedBody    bool
	Log             bool
}

// BindingPhaseDescriber lets config-aware plugins describe the one request
// stage and response phases selected by their initialized configuration.
type BindingPhaseDescriber interface {
	DescribeBindingPhases() (BindingPhaseDescriptor, error)
}
