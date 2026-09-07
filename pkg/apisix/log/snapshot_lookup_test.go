package log

import (
	"net/http"
	"net/url"
	"testing"
)

func TestLookupValueFromSnapshotPreservesEmptyDynamicValues(t *testing.T) {
	snapshot := LogSnapshot{Request: RequestLogSnapshot{
		Header: http.Header{"X-Empty": {""}}, Query: url.Values{"empty": {""}},
		RequestVars: map[string]any{"$present": "", "$nil": nil},
	}}
	for _, name := range []string{"$http_x_empty", "$arg_empty", "$present"} {
		if value, found := LookupValueFromSnapshot(snapshot, name); !found || value != "" {
			t.Errorf("%s=%v/%v", name, value, found)
		}
	}
	for _, name := range []string{"$http_missing", "$arg_missing", "$missing", "$nil"} {
		if value, found := LookupValueFromSnapshot(snapshot, name); found {
			t.Errorf("%s=%v unexpectedly found", name, value)
		}
	}
	fields := GetFieldsFromSnapshot(snapshot, map[string]string{"missing": "$missing", "nil": "$nil"})
	if fields["missing"] != "" || fields["nil"] != nil {
		t.Fatalf("legacy field resolution changed: %#v", fields)
	}
}

func TestLookupValueFromSnapshotAbsentNullableBuiltins(t *testing.T) {
	for _, name := range []string{"args", "query_string", "consumer_name", "consumer_group_id", "request_body", "response_body", "content_type", "content_length"} {
		if value, found := LookupValueFromSnapshot(LogSnapshot{}, name); found {
			t.Errorf("%s=%v unexpectedly found", name, value)
		}
		if value := ValueFromSnapshot(LogSnapshot{}, name); value != "" {
			t.Errorf("legacy %s=%v", name, value)
		}
	}
}
