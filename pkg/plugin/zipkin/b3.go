package zipkin

import (
	"errors"
	"net/http"
	"strings"
)

func extractB3(r *http.Request) (b3Context, error) {
	if b3 := r.Header.Get("b3"); b3 != "" {
		return parseSingleB3(b3)
	}

	ctx := b3Context{
		TraceID:      strings.ToLower(r.Header.Get("x-b3-traceid")),
		SpanID:       strings.ToLower(r.Header.Get("x-b3-spanid")),
		ParentSpanID: strings.ToLower(r.Header.Get("x-b3-parentspanid")),
		Sampled:      normalizeSampled(r.Header.Get("x-b3-sampled")),
	}
	if r.Header.Get("x-b3-flags") == "1" {
		ctx.Sampled = "1"
		ctx.Debug = true
	}
	if err := validateB3IDs(ctx); err != nil {
		return b3Context{}, err
	}
	return ctx, nil
}

func parseSingleB3(header string) (b3Context, error) {
	if header == "0" || header == "1" || header == "d" {
		sampled := header
		debug := sampled == "d"
		if sampled == "d" {
			sampled = "1"
		}
		return b3Context{Sampled: sampled, Debug: debug}, nil
	}

	parts := strings.Split(header, "-")
	if len(parts) < 2 {
		return b3Context{}, errors.New("invalid b3 header: missing span id")
	}
	if len(parts) > 4 {
		return b3Context{}, errors.New("invalid b3 header: too many fields")
	}

	ctx := b3Context{
		TraceID:      strings.ToLower(parts[0]),
		SpanID:       strings.ToLower(parts[1]),
		ParentSpanID: "",
	}
	if len(parts) >= 3 {
		ctx.Sampled = normalizeSampled(parts[2])
		ctx.Debug = strings.EqualFold(parts[2], "d")
	}
	if len(parts) == 4 {
		ctx.ParentSpanID = strings.ToLower(parts[3])
	}
	if err := validateB3IDs(ctx); err != nil {
		return b3Context{}, err
	}
	return ctx, nil
}

func normalizeSampled(value string) string {
	switch strings.ToLower(value) {
	case "1", "true", "d":
		return "1"
	case "0", "false":
		return "0"
	default:
		return ""
	}
}

func validateB3IDs(ctx b3Context) error {
	if ctx.TraceID != "" && !validTraceID(ctx.TraceID) {
		return errors.New("invalid b3 header: invalid trace id")
	}
	if ctx.SpanID != "" && !validSpanID(ctx.SpanID) {
		return errors.New("invalid b3 header: invalid span id")
	}
	if ctx.ParentSpanID != "" && !validSpanID(ctx.ParentSpanID) {
		return errors.New("invalid b3 header: invalid parent span id")
	}
	return nil
}

func validTraceID(value string) bool {
	return (len(value) == 16 || len(value) == 32) && hexIDPattern.MatchString(value)
}

func validSpanID(value string) bool {
	return len(value) == 16 && hexIDPattern.MatchString(value)
}

func injectB3(r *http.Request, ctx b3Context) {
	r.Header.Set("x-b3-traceid", ctx.TraceID)
	r.Header.Set("x-b3-spanid", ctx.SpanID)
	r.Header.Set("x-b3-sampled", ctx.Sampled)
	if ctx.Debug {
		r.Header.Set("x-b3-flags", "1")
	}
	if ctx.ParentSpanID != "" {
		r.Header.Set("x-b3-parentspanid", ctx.ParentSpanID)
	} else {
		r.Header.Del("x-b3-parentspanid")
	}
	r.Header.Del("b3")
}
