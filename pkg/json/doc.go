// Package json is the codec boundary for project-owned generic JSON.
//
// Production code should use this package instead of encoding/json so the
// implementation can be changed in one place. Specialized formats such as
// protobuf JSON and JSON Schema remain owned by their dedicated libraries.
package json
