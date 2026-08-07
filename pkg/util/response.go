package util

import (
	"net/http"

	"github.com/wklken/apisix-go/pkg/json"
)

// TODO: use a pool here?

func BuildMessageResponse(message string) string {
	body, _ := json.Marshal(map[string]string{
		"message": message,
	})

	return BytesToString(body)
}

func WriteJSON(w http.ResponseWriter, status int, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return nil
}

func WriteJSONMessage(w http.ResponseWriter, status int, message string) error {
	return WriteJSON(w, status, map[string]string{"message": message})
}
