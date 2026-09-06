package chaitin_waf

import "github.com/wklken/apisix-go/pkg/json"

func (cfg *Config) UnmarshalJSON(data []byte) error {
	type plain Config
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*cfg = Config(decoded)
	_, cfg.configSet = fields["config"]
	return nil
}
