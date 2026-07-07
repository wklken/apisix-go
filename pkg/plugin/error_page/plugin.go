package error_page

import (
	"bytes"
	"fmt"
	"net/http"

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
	Error404 ErrorPage `json:"error_404,omitempty"`
	Error500 ErrorPage `json:"error_500,omitempty"`
	Error502 ErrorPage `json:"error_502,omitempty"`
	Error503 ErrorPage `json:"error_503,omitempty"`
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

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := newResponseRecorder()
		next.ServeHTTP(recorder, r)

		p.rewrite(recorder)
		recorder.writeTo(w)
	})
}

func (p *Plugin) rewrite(resp *responseRecorder) {
	if !p.metadata.Enable || resp.statusCode < http.StatusNotFound {
		return
	}
	page, ok := p.errorPage(resp.statusCode)
	if !ok || page.Body == "" {
		return
	}
	resp.body.Reset()
	resp.body.WriteString(page.Body)
	resp.header.Set("Content-Type", page.ContentType)
	resp.header.Set("Content-Length", fmt.Sprint(len(page.Body)))
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
	store.GetPluginMetadata(name, &metadata)
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

type responseRecorder struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{
		header:     make(http.Header),
		statusCode: http.StatusOK,
	}
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	if r.wroteHeader {
		return
	}
	r.statusCode = statusCode
	r.wroteHeader = true
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(body)
}

func (r *responseRecorder) writeTo(w http.ResponseWriter) {
	for field, values := range r.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(r.statusCode)
	w.Write(r.body.Bytes())
}
