package base

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	apisixvar "github.com/wklken/apisix-go/pkg/apisix/variable"
)

func ReadRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, err
}

func WriteJSONMessage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
}

func RemoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

func RequestVarFromNginx(r *http.Request, key string) string {
	key = strings.TrimPrefix(key, "$")
	if after, ok := strings.CutPrefix(key, "http_"); ok {
		return r.Header.Get(strings.ReplaceAll(after, "_", "-"))
	}

	value := apisixvar.GetNginxVar(r, "$"+key)
	if key == "remote_addr" {
		return RemoteIP(value)
	}
	return value
}
