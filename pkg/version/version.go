// Package version exposes build-time version metadata for the whole project.
package version

import "runtime"

// APISIXVersion is the HTTP data-plane compatibility target exposed on APISIX wire surfaces.
const APISIXVersion = "3.17.0"

var (
	Version   = "0.1.0"
	Commit    = "none"
	BuildTime = "unknown"
	GoVersion = runtime.Version()
)
