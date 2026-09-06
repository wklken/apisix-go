package workflow

import "testing"

func TestParityOfficialAcceptsInformationalWorkflowReturn(t *testing.T) {
	p := &Plugin{config: Config{Rules: []Rule{{
		Actions: []Action{{
			Name:   "return",
			Return: ReturnAction{Code: 199},
		}},
	}}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.ValidatePreMaterialization(); err != nil {
		t.Fatalf("return code 199 rejected; APISIX 3.17 return schema minimum is 100: %v", err)
	}
}
