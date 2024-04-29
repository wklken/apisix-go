package util

import "github.com/wklken/apisix-go/pkg/json"

func BuildMessageResponse(message string) string {
	body, _ := json.Marshal(map[string]string{
		"message": message,
	})

	return BytesToString(body)
}
