package base

import (
	"strings"
	"testing"
)

func TestEncodeLogBatchPreservesBatchBoundaries(t *testing.T) {
	tests := []struct {
		name         string
		entries      []map[string]any
		batchMaxSize int
		originKey    string
		want         string
	}{
		{
			name:         "single configured entry is an object",
			entries:      []map[string]any{{"id": 1}},
			batchMaxSize: 1,
			want:         `{"id":1}`,
		},
		{
			name:         "one entry in a larger batch is an array",
			entries:      []map[string]any{{"id": 1}},
			batchMaxSize: 2,
			want:         `[{"id":1}]`,
		},
		{
			name:         "multiple entries are an array",
			entries:      []map[string]any{{"id": 1}, {"id": 2}},
			batchMaxSize: 1,
			want:         `[{"id":1},{"id":2}]`,
		},
		{
			name:         "single origin entry reuses raw payload",
			entries:      []map[string]any{{"__origin": "GET / HTTP/1.1\r\n"}},
			batchMaxSize: 1,
			originKey:    "__origin",
			want:         "GET / HTTP/1.1\r\n",
		},
		{
			name: "origin batch is a string array",
			entries: []map[string]any{
				{"__origin": "first"},
				{"__origin": "second"},
			},
			batchMaxSize: 2,
			originKey:    "__origin",
			want:         `["first","second"]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EncodeLogBatch(tt.entries, tt.batchMaxSize, tt.originKey)
			if err != nil {
				t.Fatalf("EncodeLogBatch() error = %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("EncodeLogBatch() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEncodeLogBatchReturnsMarshalError(t *testing.T) {
	_, err := EncodeLogBatch([]map[string]any{{"bad": make(chan int)}}, 1, "")
	if err == nil {
		t.Fatal("EncodeLogBatch() error = nil, want unsupported type error")
	}
	if !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("EncodeLogBatch() error = %v, want unsupported type context", err)
	}
}

func TestOriginLogEntriesRequiresEveryEntryToContainAString(t *testing.T) {
	if got, ok := OriginLogEntries(nil, "__origin"); ok || got != nil {
		t.Fatalf("OriginLogEntries(nil) = %v, %t; want nil, false", got, ok)
	}
	entries := []map[string]any{{"__origin": "first"}, {"other": "second"}}
	if got, ok := OriginLogEntries(entries, "__origin"); ok || got != nil {
		t.Fatalf("OriginLogEntries(mixed) = %v, %t; want nil, false", got, ok)
	}
}
