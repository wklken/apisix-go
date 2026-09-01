package limitbase

import (
	"testing"
	"time"
)

func TestSharedStateFixedWindowSurvivesPluginReplacement(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	state := newState(func() time.Time { return now })

	first := state.FixedWindow("plugin-limit-count/routes/r1:1:client", 3, 1, time.Minute, true)
	second := state.FixedWindow("plugin-limit-count/routes/r1:1:client", 3, 1, time.Minute, true)
	if !first.Allowed || first.Remaining != 2 || !second.Allowed || second.Remaining != 1 {
		t.Fatalf("shared fixed window results = %#v, %#v", first, second)
	}
	peek := state.FixedWindow("plugin-limit-count/routes/r1:1:client", 3, 1, time.Minute, false)
	if !peek.Allowed || peek.Remaining != 0 {
		t.Fatalf("dry-run result = %#v, want hypothetical remaining 0 without mutation", peek)
	}
	third := state.FixedWindow("plugin-limit-count/routes/r1:1:client", 3, 1, time.Minute, true)
	rejected := state.FixedWindow("plugin-limit-count/routes/r1:1:client", 3, 1, time.Minute, true)
	if !third.Allowed || third.Remaining != 0 || rejected.Allowed || rejected.Remaining != -1 {
		t.Fatalf("fixed window terminal results = %#v, %#v", third, rejected)
	}

	now = now.Add(time.Minute)
	reset := state.FixedWindow("plugin-limit-count/routes/r1:1:client", 3, 1, time.Minute, true)
	if !reset.Allowed || reset.Remaining != 2 {
		t.Fatalf("expired fixed window result = %#v, want fresh quota", reset)
	}
}

func TestSharedStateConnectionReservationSurvivesPluginReplacement(t *testing.T) {
	state := NewState()
	first := state.AcquireConnection("clientroute1", 2)
	second := state.AcquireConnection("clientroute1", 2)
	rejected := state.AcquireConnection("clientroute1", 2)
	if !first.Allowed || first.Current != 1 || !second.Allowed || second.Current != 2 || rejected.Allowed {
		t.Fatalf("connection reservations = %#v, %#v, %#v", first, second, rejected)
	}
	state.ReleaseConnection("clientroute1")
	again := state.AcquireConnection("clientroute1", 2)
	if !again.Allowed || again.Current != 2 {
		t.Fatalf("connection reservation after release = %#v", again)
	}
	state.ReleaseConnection("clientroute1")
	state.ReleaseConnection("clientroute1")
	if got := state.ConnectionCount("clientroute1"); got != 0 {
		t.Fatalf("connection count after final release = %d, want 0", got)
	}
}

func TestSharedStateCloseClearsOwnedState(t *testing.T) {
	state := NewState()
	state.FixedWindow("counter", 2, 1, time.Minute, true)
	state.AcquireConnection("connection", 1)
	state.Close()
	if got := state.ConnectionCount("connection"); got != 0 {
		t.Fatalf("connection count after Close = %d", got)
	}
	if result := state.FixedWindow("counter", 2, 1, time.Minute, true); result.Allowed {
		t.Fatalf("closed state accepted fixed-window mutation: %#v", result)
	}
}

func TestSharedStateFixedWindowSnapshotAndCredit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	state := NewStateWithClock(func() time.Time { return now })
	state.FixedWindow("ai", 10, 6, time.Minute, true)

	snapshot := state.FixedWindowSnapshot("ai", 10, time.Minute)
	if !snapshot.Exists || snapshot.Remaining != 4 || snapshot.Reset != time.Minute {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	credited := state.AdjustFixedWindow("ai", 10, -2, time.Minute, false)
	if !credited.Exists || credited.Remaining != 6 {
		t.Fatalf("credited window = %#v", credited)
	}

	now = now.Add(time.Minute)
	expired := state.FixedWindowSnapshot("ai", 10, time.Minute)
	if expired.Exists || expired.Remaining != 10 || expired.Reset != time.Minute {
		t.Fatalf("expired snapshot = %#v", expired)
	}
}

func TestSharedStateFixedWindowsAreBoundedAndExpired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	state := newStateWithCapacity(func() time.Time { return now }, 2)
	for _, key := range []string{"first", "second", "third"} {
		state.FixedWindow(key, 1, 1, time.Minute, true)
	}
	if got := state.FixedWindowCount(); got != 2 {
		t.Fatalf("fixed window count at capacity = %d, want 2", got)
	}

	now = now.Add(time.Minute)
	if got := state.FixedWindowCount(); got != 0 {
		t.Fatalf("fixed window count after expiry = %d, want 0", got)
	}
}

func TestSharedStateLeakyBucketMatchesAPISIXFirstRequestAndRejectSemantics(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	state := newState(func() time.Time { return now })

	first := state.LeakyBucket("client", 2, 0)
	if !first.Allowed || first.Delay != 0 {
		t.Fatalf("first admission = %#v, want immediate allow", first)
	}
	rejected := state.LeakyBucket("client", 2, 0)
	if rejected.Allowed {
		t.Fatalf("immediate second admission = %#v, want rejection", rejected)
	}

	now = now.Add(500 * time.Millisecond)
	recovered := state.LeakyBucket("client", 2, 0)
	if !recovered.Allowed || recovered.Delay != 0 {
		t.Fatalf("recovered admission = %#v, want immediate allow", recovered)
	}
}

func TestSharedStateLeakyBucketSurvivesCallersAndRefreshesExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	state := newState(func() time.Time { return now })

	if result := state.LeakyBucket("client", 1, 1); !result.Allowed {
		t.Fatalf("first admission = %#v", result)
	}
	second := state.LeakyBucket("client", 1, 1)
	if !second.Allowed || second.Delay != time.Second {
		t.Fatalf("second admission = %#v, want one-second delay", second)
	}
	if result := state.LeakyBucket("client", 1, 1); result.Allowed {
		t.Fatalf("third admission = %#v, want rejection", result)
	}

	now = now.Add(2 * time.Second)
	if result := state.LeakyBucket("client", 1, 1); !result.Allowed || result.Delay != 0 {
		t.Fatalf("expired bucket admission = %#v, want fresh state", result)
	}
}
