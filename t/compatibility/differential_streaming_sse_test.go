package pluginintegration

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestObserveDifferentialSSESeesThreeFramesBeforeEOF(t *testing.T) {
	contract := differentialSSEStreamContract{
		Frames: []string{
			"data: event-1\n\n",
			"data: event-2\n\n",
			"data: event-3\n\n",
		},
		RequiredFrames:  3,
		InterFrameDelay: 5 * time.Millisecond,
		OpenProbeWindow: 20 * time.Millisecond,
	}
	fixture, err := newDifferentialSSEFixture(contract)
	if err != nil {
		t.Fatalf("start SSE fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := fixture.Close(); err != nil {
			t.Errorf("close SSE fixture: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	observation, err := observeDifferentialSSE(ctx, fixture.Address(), DifferentialRequest{
		Method: http.MethodGet,
		Path:   "/events",
		Host:   "gateway.example.test",
		Headers: map[string]string{
			"Accept": "text/event-stream",
		},
	}, contract)
	if err != nil {
		t.Fatalf("observe SSE stream: %v", err)
	}
	if observation.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", observation.Status)
	}
	if observation.ContentType != "text/event-stream" {
		t.Fatalf("Content-Type = %q", observation.ContentType)
	}
	if !reflect.DeepEqual(observation.Frames, contract.Frames) {
		t.Fatalf("frames = %#v, want %#v", observation.Frames, contract.Frames)
	}
	if !observation.ConnectionOpenAfterRequiredFrames {
		t.Fatal("connection reached EOF before the third frame could be observed incrementally")
	}
}

func TestDifferentialProxyBufferingSSESharedFixtureFlushesBeforeEOF(t *testing.T) {
	streamCase := newDifferentialProxyBufferingStreamCase(differentialCasesForPlugin("proxy-buffering")[0])
	fixture, err := newDifferentialFixture(streamCase.Spec.Fixture)
	if err != nil {
		t.Fatalf("start shared SSE fixture: %v", err)
	}
	t.Cleanup(fixture.close)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	observation, err := observeDifferentialSSE(
		ctx,
		fixture.listener.Addr().String(),
		streamCase.Spec.Request,
		streamCase.Contract,
	)
	if err != nil {
		t.Fatalf("observe shared SSE fixture: %v", err)
	}
	if !reflect.DeepEqual(observation.Frames, streamCase.Contract.Frames) ||
		!observation.ConnectionOpenAfterRequiredFrames {
		t.Fatalf("shared SSE observation = %#v", observation)
	}
}

func TestParseDifferentialSSEOraclePacketPreservesFramesAndOpenProof(t *testing.T) {
	streamCase := newDifferentialProxyBufferingStreamCase(differentialCasesForPlugin("proxy-buffering")[0])
	var packet strings.Builder
	packet.WriteString(differentialSSEOraclePacketMagic + "\n200\n")
	packet.WriteString(hex.EncodeToString([]byte("text/event-stream")))
	packet.WriteString("\n1\n3\n")
	for _, frame := range streamCase.Contract.Frames {
		packet.WriteString(hex.EncodeToString([]byte(frame)))
		packet.WriteByte('\n')
	}
	observation, err := parseDifferentialSSEOraclePacket([]byte(packet.String()))
	if err != nil {
		t.Fatalf("parse Oracle packet: %v", err)
	}
	if !reflect.DeepEqual(observation.Frames, streamCase.Contract.Frames) ||
		!observation.ConnectionOpenAfterRequiredFrames {
		t.Fatalf("Oracle observation = %#v", observation)
	}
	if _, err := parseDifferentialSSEOraclePacket(fmt.Appendf(
		nil, "%s\n200\n00\n2\n0\n", differentialSSEOraclePacketMagic,
	)); err == nil {
		t.Fatal("parser accepted an invalid open flag")
	}
}

func TestObserveDifferentialSSERejectsEOFBeforeRequiredFrames(t *testing.T) {
	contract := differentialSSEStreamContract{
		Frames: []string{
			"data: event-1\n\n",
			"data: event-2\n\n",
		},
		RequiredFrames:   3,
		OpenProbeWindow:  10 * time.Millisecond,
		CloseAfterFrames: true,
	}
	fixture, err := newDifferentialSSEFixture(contract)
	if err != nil {
		t.Fatalf("start SSE fixture: %v", err)
	}
	t.Cleanup(func() { _ = fixture.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = observeDifferentialSSE(ctx, fixture.Address(), DifferentialRequest{
		Method: http.MethodGet,
		Path:   "/events",
		Host:   "gateway.example.test",
	}, contract)
	if err == nil {
		t.Fatal("observe SSE stream succeeded after upstream EOF with only two of three required frames")
	}
}

func TestDifferentialSSEDriverOutputRoundTripsStrictly(t *testing.T) {
	want := differentialSSEStreamObservation{
		Status:                            http.StatusOK,
		ContentType:                       "text/event-stream",
		Frames:                            []string{"data: event-1\n\n"},
		ConnectionOpenAfterRequiredFrames: true,
	}
	raw, err := encodeDifferentialSSEDriverOutput(want)
	if err != nil {
		t.Fatalf("encode driver output: %v", err)
	}
	got, err := parseDifferentialSSEDriverOutput(raw)
	if err != nil {
		t.Fatalf("parse driver output: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("driver output = %#v, want %#v", got, want)
	}
	if _, err := parseDifferentialSSEDriverOutput(append(raw, []byte(` {}`)...)); err == nil {
		t.Fatal("parser accepted multiple JSON values")
	}
}
