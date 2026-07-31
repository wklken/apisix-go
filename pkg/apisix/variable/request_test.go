package variable

import "testing"

func TestRequestVarsRegistersRateLimitingInfo(t *testing.T) {
	if _, ok := RequestVars["$rate_limiting_info"]; !ok {
		t.Fatal("$rate_limiting_info is not registered as a request variable")
	}
}
