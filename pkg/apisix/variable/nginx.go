package variable

import "net/http"

// nginx vars: http://nginx.org/en/docs/varindex.html

var NginxVars = map[string]struct{}{
	"request_method":       {},
	"request_uri":          {},
	"remote_addr":          {},
	"http_host":            {},
	"uri":                  {},
	"args":                 {},
	"query_string":         {},
	"http_user_agent":      {},
	"http_referer":         {},
	"server_protocol":      {},
	"http_x_forwarded_for": {},
	"scheme":               {},
	"content-length":       {},
	"content-type":         {},
}

func GetNginxVar(r *http.Request, key string) string {
	switch key {
	case "request_method":
		return r.Method
	case "request_uri":
		return r.URL.RequestURI()
	case "remote_addr":
		return r.RemoteAddr
	case "http_host":
		return r.Host
	case "uri":
		return r.URL.Path
	case "args", "query_string":
		return r.URL.RawQuery
	case "http_user_agent":
		return r.UserAgent()
	case "http_referer":
		return r.Referer()
	case "server_protocol":
		return r.Proto
	case "http_x_forwarded_for":
		return r.Header.Get("X-Forwarded-For")
	case "scheme":
		if r.TLS != nil {
			return "https"
		}
		return "http"
	case "content_length":
		return r.Header.Get("Content-Length")
	case "content_type":
		return r.Header.Get("Content-Type")
	default:
		return ""

	}
}
