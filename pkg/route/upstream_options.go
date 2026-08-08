package route

import (
	"time"

	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

type upstreamTimeouts struct {
	connect        time.Duration
	send           time.Duration
	read           time.Duration
	responseHeader time.Duration
}

func resolveUpstreamTimeouts(routeTimeout, upstreamTimeout resource.Timeout) upstreamTimeouts {
	resolved := upstreamTimeout
	if routeTimeout.Connect > 0 {
		resolved.Connect = routeTimeout.Connect
	}
	if routeTimeout.Send > 0 {
		resolved.Send = routeTimeout.Send
	}
	if routeTimeout.Read > 0 {
		resolved.Read = routeTimeout.Read
	}
	connect := proxy.DefaultDialTimeout
	if resolved.Connect > 0 {
		connect = time.Duration(resolved.Connect) * time.Second
	}
	read := time.Duration(resolved.Read) * time.Second
	return upstreamTimeouts{
		connect:        connect,
		send:           time.Duration(resolved.Send) * time.Second,
		read:           read,
		responseHeader: read,
	}
}

func upstreamTLSInsecureSkipVerify(upstream resource.Upstream) bool {
	return upstream.TLS == nil || !upstream.TLS.Verify
}
