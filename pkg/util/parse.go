package util

import "github.com/wklken/apisix-go/pkg/json"

func Parse(source interface{}, dest interface{}) error {
	j, err := json.Marshal(source)
	if err != nil {
		return err
	}

	err = json.Unmarshal(j, dest)
	if err != nil {
		return err
	}
	return nil
}
