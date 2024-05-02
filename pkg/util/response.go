package util

import "github.com/wklken/apisix-go/pkg/json"

// TODO: use a pool here?

func BuildMessageResponse(message string) string {
	body, _ := json.Marshal(map[string]string{
		"message": message,
	})

	return BytesToString(body)
}
