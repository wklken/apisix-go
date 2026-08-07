package shared

import (
	"net/http"
	"testing"
)

func TestLoadOrStoreClientReusesSameUIDAndSeparatesDifferent(t *testing.T) {
	uid := NewConfigUID()
	uid.Add("test", 1)
	otherUID := NewConfigUID()
	otherUID.Add("test", 2)

	first := &http.Client{}
	second := &http.Client{}
	other := &http.Client{}

	gotFirst := LoadOrStoreClient(t.Name(), uid, first)
	gotSecond := LoadOrStoreClient(t.Name(), uid, second)
	if gotFirst != first {
		t.Fatal("first LoadOrStoreClient did not return the stored client")
	}
	if gotSecond != first {
		t.Fatal("second LoadOrStoreClient for the same UID returned a different client")
	}

	gotOther := LoadOrStoreClient(t.Name(), otherUID, other)
	if gotOther != other || gotOther == gotFirst {
		t.Fatal("LoadOrStoreClient for a different UID did not retain its own client")
	}
}
