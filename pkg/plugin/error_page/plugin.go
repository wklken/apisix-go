package error_page

import (
	"fmt"
	"net/http"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
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

	return nil
}

func (p *Plugin) PostInit() error {
	if !p.metadata.Enable {
		p.metadata = p.loadMetadata()
	}
	applyDefaults(&p.metadata)
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	resp.Header().Set("Content-Length", fmt.Sprint(len(page.Body)))
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

func (p *Plugin) loadMetadata() (metadata Metadata) {
	defer func() {
		if recover() != nil {
			metadata = Metadata{}
		}
	}()
	_ = store.GetPluginMetadata(name, &metadata)
	return metadata
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
