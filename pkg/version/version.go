// Package version exposes build-time version metadata for the whole project.
package version

import "runtime"

var (
	Version   = "0.1.0"
	Commit    = "none"
	BuildTime = "unknown"
	GoVersion = runtime.Version()
)
