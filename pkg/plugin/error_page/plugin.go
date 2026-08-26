package error_page

import (
	"fmt"
	"net/http"
	"strconv"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config   Config
	metadata Metadata
}

const (
	priority = 450
	name     = "error-page"
)

const schema = `{}`

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "enable": {
      "type": "boolean"
    },
    "error_404": {
      "type": "object",
      "properties": {
        "body": {"type": "string"},
        "content_type": {"type": "string"}
      }
    },
    "error_500": {
      "type": "object",
      "properties": {
        "body": {"type": "string"},
        "content_type": {"type": "string"}
      }
    },
    "error_502": {
      "type": "object",
      "properties": {
        "body": {"type": "string"},
        "content_type": {"type": "string"}
      }
    },
    "error_503": {
      "type": "object",
      "properties": {
        "body": {"type": "string"},
        "content_type": {"type": "string"}
      }
    }
  }
}`

type Config struct{}

type Metadata struct {
	Enable   bool      `json:"enable,omitempty"`
	Error404 ErrorPage `json:"error_404"`
	Error500 ErrorPage `json:"error_500"`
	Error502 ErrorPage `json:"error_502"`
	Error503 ErrorPage `json:"error_503"`
}

type ErrorPage struct {
	Body        string `json:"body,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.MetadataSchema = metadataSchema

	return nil
}

func (p *Plugin) PostInit() error {
	metadata, err := p.loadMetadata()
	if err != nil {
		return err
	}
	p.metadata = metadata
	applyDefaults(&p.metadata)
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) RunBufferedBodyFilter(r *http.Request, state *base.ResponseState) error {
	if state == nil || !p.metadata.Enable || state.Status < http.StatusNotFound ||
		!p.AppliesToResponseSource(responseSource(r)) {
		return nil
	}
	page, ok := p.errorPage(state.Status)
	if !ok || page.Body == "" {
		return nil
	}
	if state.Header == nil {
		state.Header = make(http.Header)
	}
	state.Body = []byte(page.Body)
	base.InvalidateBodyDerivedHeaders(state.Header)
	state.Header.Set("Content-Type", page.ContentType)
	state.Header.Set("Content-Length", strconv.Itoa(len(page.Body)))
	return nil
}

func (p *Plugin) AppliesToResponseSource(source apisixctx.ResponseSource) bool {
	return source == apisixctx.ResponseSourceAPISIX || source == apisixctx.ResponseSourceEarlyStop
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apisixctx.GetRequestLifecycle(r) != nil {
			next.ServeHTTP(w, r)
			return
		}
		recorder := base.GetOrCreateTransformResponseWriter(r)
		next.ServeHTTP(recorder, r)

		p.rewrite(r, recorder)
		recorder.Commit(w)
	})
}

func (p *Plugin) rewrite(r *http.Request, resp *base.BufferedResponseWriter) {
	if !p.metadata.Enable || resp.StatusCode() < http.StatusNotFound {
		return
	}
	if source, _ := apisixctx.GetRequestVar(r, "$response_source").(string); source == "upstream" {
		return
	}
	page, ok := p.errorPage(resp.StatusCode())
	if !ok || page.Body == "" {
		return
	}
	resp.SetBody([]byte(page.Body))
	resp.Header().Set("Content-Type", page.ContentType)
	resp.Header().Set("Content-Length", strconv.Itoa(len(page.Body)))
}

func responseSource(r *http.Request) apisixctx.ResponseSource {
	if lifecycle := apisixctx.GetRequestLifecycle(r); lifecycle != nil {
		return lifecycle.ResponseSource()
	}
	if source, _ := apisixctx.GetRequestVar(r, "$response_source").(string); source != "" {
		return apisixctx.ResponseSource(source)
	}
	return apisixctx.ResponseSourceUnknown
}

func (p *Plugin) errorPage(statusCode int) (ErrorPage, bool) {
	switch statusCode {
	case http.StatusNotFound:
		return p.metadata.Error404, true
	case http.StatusInternalServerError:
		return p.metadata.Error500, true
	case http.StatusBadGateway:
		return p.metadata.Error502, true
	case http.StatusServiceUnavailable:
		return p.metadata.Error503, true
	default:
		return ErrorPage{}, false
	}
}

func (p *Plugin) loadMetadata() (Metadata, error) {
	var metadata Metadata
	if _, err := p.MetadataView().Decode(name, &metadata); err != nil {
		return Metadata{}, fmt.Errorf("%s metadata decode failed: %w", name, err)
	}
	return metadata, nil
}

func applyDefaults(metadata *Metadata) {
	defaultErrorPage(&metadata.Error404, "404 Not Found")
	defaultErrorPage(&metadata.Error500, "500 Internal Server Error")
	defaultErrorPage(&metadata.Error502, "502 Bad Gateway")
	defaultErrorPage(&metadata.Error503, "503 Service Unavailable")
}

func defaultErrorPage(page *ErrorPage, title string) {
	if page.ContentType == "" {
		page.ContentType = "text/html"
	}
	if page.Body == "" {
		page.Body = fmt.Sprintf(`<html>
<head><title>%s</title></head>
<body>
<center><h1>%s</h1></center>
<hr><center>Apache APISIX</center>
</body>
</html>`, title, title)
	}
}
