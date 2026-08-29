package pluginintegration

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPrepareDifferentialLogRotateSidesSeedsIndependentTrees(t *testing.T) {
	plan := differentialLogRotatePlan()
	sides, err := prepareDifferentialLogRotateSides(t.TempDir(), plan)
	if err != nil {
		t.Fatalf("prepareDifferentialLogRotateSides() error = %v", err)
	}
	if sides.CandidateDir == sides.OracleHostDir || !filepath.IsAbs(sides.CandidateDir) ||
		!filepath.IsAbs(sides.OracleHostDir) {
		t.Fatalf("side directories = %#v", sides)
	}
	if sides.OracleConfigDir != differentialOracleFileDirectory {
		t.Fatalf("oracle config directory = %q", sides.OracleConfigDir)
	}
	for _, root := range []string{sides.CandidateDir, sides.OracleHostDir} {
		for relative, want := range map[string]string{
			"logs/access.log": differentialLogRotateSeedContent(),
			"logs/2020-01-01_00-00-00__access.log.tar.gz": "old-one",
			"logs/2021-01-01_00-00-00__access.log.tar.gz": "old-two",
			"logs/unrelated-sentinel.txt":                 "keep-me\n",
		} {
			body, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
			if readErr != nil || string(body) != want {
				t.Fatalf("seed %s/%s = %q/%v, want %q", root, relative, body, readErr, want)
			}
		}
	}
}

func TestDifferentialLogRotatePlanTriggersOnlyTheOversizedSeed(t *testing.T) {
	plan := differentialLogRotatePlan()
	if len(differentialLogRotateSeedContent()) <= plan.MaxSize {
		t.Fatalf(
			"log-rotate seed size = %d, want greater than max_size %d",
			len(differentialLogRotateSeedContent()),
			plan.MaxSize,
		)
	}
	if plan.MaxSize < 1024 {
		t.Fatalf("log-rotate max_size = %d, want enough room for the two post-rotation request records", plan.MaxSize)
	}
}

func TestDifferentialLogRotatePlanAllowsAPISIXSizeTimerBoundary(t *testing.T) {
	if got := differentialLogRotatePlan().WaitTimeout; got <= 5*time.Second {
		t.Fatalf("log-rotate wait timeout = %s, want slack beyond APISIX's observed five-second size timer", got)
	}
}

func TestDifferentialLogRotateRuntimeAndRouteProjectionUseSideDirectory(t *testing.T) {
	sideDir := "/private/differential/log-rotate"
	runtime := differentialLogRotateRuntimeOverlay(sideDir)
	attr := runtime["plugin_attr"].(map[string]any)["log-rotate"].(map[string]any)
	wantAttr := map[string]any{
		"interval": 86400, "max_size": 1024, "max_kept": 1,
		"enable_compression": true, "timeout": 10000,
	}
	if !reflect.DeepEqual(attr, wantAttr) {
		t.Fatalf("log-rotate runtime attr = %#v, want %#v", attr, wantAttr)
	}
	nginx := runtime["nginx_config"].(map[string]any)
	if nginx["error_log"] != "/dev/null" {
		t.Fatalf("nginx error log = %#v", nginx["error_log"])
	}
	httpConfig := nginx["http"].(map[string]any)
	if httpConfig["access_log"] != sideDir+"/logs/access.log" || httpConfig["enable_access_log"] != true {
		t.Fatalf("nginx http log config = %#v", httpConfig)
	}

	projected, err := projectDifferentialLogRotateConfig(differentialLogRotateCases()[0].Config, sideDir)
	if err != nil {
		t.Fatalf("projectDifferentialLogRotateConfig() error = %v", err)
	}
	route := projected["routes"].([]any)[0].(map[string]any)
	fileLogger := route["plugins"].(map[string]any)["file-logger"].(map[string]any)
	if fileLogger["path"] != sideDir+"/logs/access.log" {
		t.Fatalf("projected file-logger path = %#v", fileLogger["path"])
	}
}

func TestCollectDifferentialLogRotateObservationValidatesCompressedPrunedReopenedState(t *testing.T) {
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(logs, "access.log"),
		[]byte(`127.0.0.1 - - "GET /after-rotate HTTP/1.1" 200`+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "unrelated-sentinel.txt"), []byte("keep-me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archiveName := "2026-08-29_12-34-56__access.log.tar.gz"
	writeDifferentialLogRotateArchiveForTest(
		t,
		filepath.Join(logs, archiveName),
		"2026-08-29_12-34-56__access.log",
		differentialLogRotatePreMarker+`{"path":"/rotate"}`+"\n",
	)

	observation, err := collectDifferentialLogRotateObservation(root, true, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("collectDifferentialLogRotateObservation() error = %v", err)
	}
	if observation.Name != differentialLogRotateObservationName || !observation.Exists ||
		observation.Truncated || observation.Size != int64(len(observation.Content)) {
		t.Fatalf("file observation = %#v", observation)
	}
	state, err := decodeDifferentialLogRotateState(observation.Content)
	if err != nil {
		t.Fatalf("decodeDifferentialLogRotateState() error = %v", err)
	}
	if state.ArchiveName != archiveName ||
		state.ArchiveMember != "2026-08-29_12-34-56__access.log" ||
		!strings.Contains(state.ArchiveContent, differentialLogRotatePreMarker) ||
		!strings.Contains(state.CurrentContent, "/after-rotate") ||
		state.SentinelContent != "keep-me\n" {
		t.Fatalf("log-rotate state = %#v", state)
	}
}

func TestCollectDifferentialLogRotateObservationRejectsUnsafeArchiveMember(t *testing.T) {
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(logs, "access.log"),
		[]byte(`{"path":"/after-rotate"}`+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "unrelated-sentinel.txt"), []byte("keep-me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeDifferentialLogRotateArchiveForTest(
		t,
		filepath.Join(logs, "2026-08-29_12-34-56__access.log.tar.gz"),
		"../access.log",
		differentialLogRotatePreMarker,
	)
	if _, err := collectDifferentialLogRotateObservation(root, true, 50*time.Millisecond); err == nil {
		t.Fatal("unsafe archive member error = nil")
	}
}

func TestCollectDifferentialLogRotateOracleObservationCapturesContainerCurrentBeforeStop(t *testing.T) {
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "access.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "unrelated-sentinel.txt"), []byte("keep-me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeDifferentialLogRotateArchiveForTest(
		t,
		filepath.Join(logs, "2026-08-29_12-34-56__access.log.tar.gz"),
		"2026-08-29_12-34-56__access.log",
		differentialLogRotatePreMarker,
	)
	runtime := filepath.Join(t.TempDir(), "fake-container")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = logs ]; then printf 'oracle-log\\n'; exit 0; fi\n" +
		"if [ \"$1\" = exec ]; then printf 'GET /after-rotate HTTP/1.1\\n'; exit 0; fi\n" +
		"if [ \"$1\" = rm ]; then exit 0; fi\n" +
		"exit 1\n"
	if err := os.WriteFile(runtime, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	diagnostic := filepath.Join(t.TempDir(), "oracle.log")
	child := &differentialChild{container: true, runtime: runtime, name: "fake-oracle"}
	observation, stopped, err := collectDifferentialLogRotateOracleObservation(
		child, diagnostic, root, 500*time.Millisecond,
	)
	if err != nil {
		t.Fatalf("collectDifferentialLogRotateOracleObservation() error = %v", err)
	}
	if !stopped || !strings.Contains(observation.Content, "/after-rotate") {
		t.Fatalf("oracle observation = %#v, stopped=%t", observation, stopped)
	}
	if body, readErr := os.ReadFile(diagnostic); readErr != nil || string(body) != "oracle-log\n" {
		t.Fatalf("oracle diagnostic = %q/%v", body, readErr)
	}
}

func writeDifferentialLogRotateArchiveForTest(t *testing.T, path, member, content string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: member, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
