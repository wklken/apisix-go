package pluginintegration

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	differentialLogRotateSideDirectoryPlaceholder = "{{SIDE.DIR}}"
	differentialLogRotateObservationName          = "log-rotate-state.json"
	differentialLogRotatePreMarker                = "rotate-me-marker\n"
	differentialLogRotatePostMarker               = "/after-rotate"
	differentialLogRotateMaxCaptureBytes          = 1 << 20
)

var differentialLogRotateArchivePattern = regexp.MustCompile(
	`^\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}__access\.log\.tar\.gz$`,
)

type differentialLogRotateFixturePlan struct {
	MaxSize     int
	WaitTimeout time.Duration
}

type differentialLogRotateSides struct {
	CandidateDir    string
	OracleHostDir   string
	OracleConfigDir string
}

type differentialLogRotateState struct {
	ArchiveName     string `json:"archive_name"`
	ArchiveMember   string `json:"archive_member"`
	ArchiveContent  string `json:"archive_content"`
	CurrentContent  string `json:"current_content"`
	SentinelContent string `json:"sentinel_content"`
}

func differentialLogRotatePlan() differentialLogRotateFixturePlan {
	return differentialLogRotateFixturePlan{MaxSize: 1024, WaitTimeout: 8 * time.Second}
}

func differentialLogRotateSeedContent() string {
	return differentialLogRotatePreMarker + strings.Repeat("x", 2048) + "\n"
}

func prepareDifferentialLogRotateSides(
	workDir string,
	plan differentialLogRotateFixturePlan,
) (differentialLogRotateSides, error) {
	if plan.MaxSize < 1 || plan.WaitTimeout <= 0 {
		return differentialLogRotateSides{}, errors.New("log-rotate fixture plan is invalid")
	}
	sides := differentialLogRotateSides{
		CandidateDir:    filepath.Join(workDir, "candidate-log-rotate"),
		OracleHostDir:   filepath.Join(workDir, "oracle-log-rotate"),
		OracleConfigDir: differentialOracleFileDirectory,
	}
	for _, side := range []struct {
		root     string
		dirMode  os.FileMode
		fileMode os.FileMode
	}{
		{root: sides.CandidateDir, dirMode: 0o700, fileMode: 0o600},
		{root: sides.OracleHostDir, dirMode: 0o777, fileMode: 0o666},
	} {
		if err := seedDifferentialLogRotateSide(side.root, side.dirMode, side.fileMode); err != nil {
			return differentialLogRotateSides{}, err
		}
	}
	return sides, nil
}

func seedDifferentialLogRotateSide(root string, dirMode, fileMode os.FileMode) error {
	logs := filepath.Join(root, "logs")
	if err := os.MkdirAll(logs, dirMode); err != nil {
		return fmt.Errorf("create log-rotate side directory: %w", err)
	}
	if err := os.Chmod(root, dirMode); err != nil {
		return fmt.Errorf("chmod log-rotate side root: %w", err)
	}
	if err := os.Chmod(logs, dirMode); err != nil {
		return fmt.Errorf("chmod log-rotate log directory: %w", err)
	}
	seeds := map[string]string{
		"access.log":                             differentialLogRotateSeedContent(),
		"2020-01-01_00-00-00__access.log.tar.gz": "old-one",
		"2021-01-01_00-00-00__access.log.tar.gz": "old-two",
		"unrelated-sentinel.txt":                 "keep-me\n",
	}
	for name, body := range seeds {
		if err := os.WriteFile(filepath.Join(logs, name), []byte(body), fileMode); err != nil {
			return fmt.Errorf("seed log-rotate file %s: %w", name, err)
		}
	}
	return nil
}

func differentialLogRotateRuntimeOverlay(sideDir string) map[string]any {
	return map[string]any{
		"plugin_attr": map[string]any{
			"log-rotate": map[string]any{
				"interval": 86400, "max_size": differentialLogRotatePlan().MaxSize,
				"max_kept": 1, "enable_compression": true, "timeout": 10000,
			},
		},
		"nginx_config": map[string]any{
			"error_log": "/dev/null",
			"http": map[string]any{
				"access_log":        filepath.ToSlash(filepath.Join(sideDir, "logs", "access.log")),
				"enable_access_log": true,
			},
		},
	}
}

func projectDifferentialLogRotateConfig(
	config map[string]any,
	sideDir string,
) (map[string]any, error) {
	if !filepath.IsAbs(sideDir) {
		return nil, fmt.Errorf("log-rotate side directory %q is not absolute", sideDir)
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("marshal log-rotate differential config: %w", err)
	}
	var projected map[string]any
	if err := yaml.Unmarshal(data, &projected); err != nil {
		return nil, fmt.Errorf("clone log-rotate differential config: %w", err)
	}
	count := replaceDifferentialLogRotateSideDirectory(projected, filepath.ToSlash(sideDir))
	if count != 1 {
		return nil, fmt.Errorf("log-rotate side directory placeholder count = %d, want 1", count)
	}
	return projected, nil
}

func replaceDifferentialLogRotateSideDirectory(value any, sideDir string) int {
	count := 0
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if text, ok := child.(string); ok && strings.Contains(text, differentialLogRotateSideDirectoryPlaceholder) {
				typed[key] = strings.ReplaceAll(text, differentialLogRotateSideDirectoryPlaceholder, sideDir)
				count++
				continue
			}
			count += replaceDifferentialLogRotateSideDirectory(child, sideDir)
		}
	case []any:
		for index, child := range typed {
			if text, ok := child.(string); ok && strings.Contains(text, differentialLogRotateSideDirectoryPlaceholder) {
				typed[index] = strings.ReplaceAll(text, differentialLogRotateSideDirectoryPlaceholder, sideDir)
				count++
				continue
			}
			count += replaceDifferentialLogRotateSideDirectory(child, sideDir)
		}
	}
	return count
}

func waitDifferentialLogRotateAfterStep(spec DifferentialCase, stepIndex int, root string) error {
	if spec.Name != "log-rotate-size-compress-prune-reopen" || stepIndex != 0 {
		return nil
	}
	_, err := collectDifferentialLogRotateObservation(
		root,
		false,
		differentialLogRotatePlan().WaitTimeout,
	)
	return err
}

func collectDifferentialLogRotateObservation(
	root string,
	requirePostMarker bool,
	timeout time.Duration,
) (DifferentialFileObservation, error) {
	if !filepath.IsAbs(root) || timeout <= 0 {
		return DifferentialFileObservation{}, errors.New(
			"log-rotate observation requires an absolute root and positive timeout",
		)
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		state, ready, err := snapshotDifferentialLogRotateState(root, requirePostMarker)
		if err == nil && ready {
			content, marshalErr := json.Marshal(state)
			if marshalErr != nil {
				return DifferentialFileObservation{}, fmt.Errorf("marshal log-rotate observation: %w", marshalErr)
			}
			return DifferentialFileObservation{
				Name: differentialLogRotateObservationName, Exists: true,
				Size: int64(len(content)), Content: string(content),
			}, nil
		}
		lastErr = err
		if !time.Now().Before(deadline) {
			if lastErr != nil {
				return DifferentialFileObservation{}, fmt.Errorf("wait for log-rotate directory state: %w", lastErr)
			}
			return DifferentialFileObservation{}, errors.New(
				"wait for log-rotate directory state: condition not reached",
			)
		}
		<-ticker.C
	}
}

func collectDifferentialLogRotateOracleObservation(
	child *differentialChild,
	diagnosticPath string,
	root string,
	timeout time.Duration,
) (DifferentialFileObservation, bool, error) {
	if child == nil {
		return DifferentialFileObservation{}, false, errors.New("log-rotate oracle child is nil")
	}
	logs, err := child.logs()
	if err != nil {
		return DifferentialFileObservation{}, false, fmt.Errorf("capture log-rotate oracle logs: %w", err)
	}
	if err := writeDifferentialDiagnosticLog(diagnosticPath, strings.TrimSpace(logs)); err != nil {
		return DifferentialFileObservation{}, false, err
	}
	current, err := readDifferentialLogRotateOracleCurrent(child, timeout)
	if err != nil {
		return DifferentialFileObservation{}, false, err
	}
	observation, err := collectDifferentialLogRotateObservation(root, false, timeout)
	if err != nil {
		return DifferentialFileObservation{}, false, err
	}
	state, err := decodeDifferentialLogRotateState(observation.Content)
	if err != nil {
		return DifferentialFileObservation{}, false, err
	}
	state.CurrentContent = current
	content, err := json.Marshal(state)
	if err != nil {
		return DifferentialFileObservation{}, false, fmt.Errorf("marshal log-rotate oracle observation: %w", err)
	}
	observation.Size = int64(len(content))
	observation.Content = string(content)
	if err := child.stop(); err != nil {
		return DifferentialFileObservation{}, true, fmt.Errorf("stop log-rotate oracle after file collection: %w", err)
	}
	return observation, true, nil
}

func readDifferentialLogRotateOracleCurrent(child *differentialChild, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		output, err := runDifferentialPodmanCommand(
			child.runtime,
			differentialPodmanTimeout,
			nil,
			nil,
			"exec",
			child.name,
			"cat",
			filepath.ToSlash(filepath.Join(differentialOracleFileDirectory, "logs", "access.log")),
		)
		current := string(output)
		if err == nil && strings.Contains(current, differentialLogRotatePostMarker) &&
			!strings.Contains(current, differentialLogRotatePreMarker) {
			return current, nil
		}
		if err != nil {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			if lastErr != nil {
				return "", fmt.Errorf("read reopened log-rotate oracle file: %w", lastErr)
			}
			return "", errors.New("read reopened log-rotate oracle file: post-rotate marker not observed")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func snapshotDifferentialLogRotateState(
	root string,
	requirePostMarker bool,
) (differentialLogRotateState, bool, error) {
	logs := filepath.Join(root, "logs")
	current, err := readDifferentialLogRotateBounded(filepath.Join(logs, "access.log"))
	if err != nil {
		return differentialLogRotateState{}, false, err
	}
	sentinel, err := readDifferentialLogRotateBounded(filepath.Join(logs, "unrelated-sentinel.txt"))
	if err != nil {
		return differentialLogRotateState{}, false, err
	}
	for _, old := range []string{
		"2020-01-01_00-00-00__access.log.tar.gz",
		"2021-01-01_00-00-00__access.log.tar.gz",
	} {
		if _, err := os.Stat(filepath.Join(logs, old)); err == nil {
			return differentialLogRotateState{}, false, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return differentialLogRotateState{}, false, err
		}
	}
	entries, err := os.ReadDir(logs)
	if err != nil {
		return differentialLogRotateState{}, false, err
	}
	var archives, plainHistory []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, "__access.log.tar.gz") {
			archives = append(archives, name)
		}
		if strings.HasSuffix(name, "__access.log") {
			plainHistory = append(plainHistory, name)
		}
	}
	if len(archives) != 1 || len(plainHistory) != 0 ||
		!differentialLogRotateArchivePattern.MatchString(archives[0]) {
		return differentialLogRotateState{}, false, nil
	}
	member, archiveContent, err := readDifferentialLogRotateArchive(
		filepath.Join(logs, archives[0]),
		strings.TrimSuffix(archives[0], ".tar.gz"),
	)
	if err != nil {
		return differentialLogRotateState{}, false, err
	}
	ready := strings.Contains(archiveContent, differentialLogRotatePreMarker) &&
		!strings.Contains(current, differentialLogRotatePreMarker) && sentinel == "keep-me\n"
	if requirePostMarker {
		ready = ready && strings.Contains(current, differentialLogRotatePostMarker)
	}
	return differentialLogRotateState{
		ArchiveName: archives[0], ArchiveMember: member, ArchiveContent: archiveContent,
		CurrentContent: current, SentinelContent: sentinel,
	}, ready, nil
}

func readDifferentialLogRotateBounded(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, differentialLogRotateMaxCaptureBytes+1))
	if err != nil {
		return "", err
	}
	if len(content) > differentialLogRotateMaxCaptureBytes {
		return "", fmt.Errorf(
			"log-rotate file %s exceeds %d bytes",
			filepath.Base(path),
			differentialLogRotateMaxCaptureBytes,
		)
	}
	return string(content), nil
}

func readDifferentialLogRotateArchive(path, wantMember string) (string, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() || info.Size() > differentialLogRotateMaxCaptureBytes {
		return "", "", fmt.Errorf("log-rotate archive is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = file.Close() }()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = gz.Close() }()
	reader := tar.NewReader(gz)
	header, err := reader.Next()
	if err != nil {
		return "", "", err
	}
	if header.Typeflag != tar.TypeReg {
		return "", "", fmt.Errorf("log-rotate archive member type = %d, want regular", header.Typeflag)
	}
	if header.Name != wantMember || filepath.Base(header.Name) != header.Name || strings.Contains(header.Name, "..") {
		return "", "", fmt.Errorf("log-rotate archive member = %q, want %q", header.Name, wantMember)
	}
	content, err := io.ReadAll(io.LimitReader(reader, differentialLogRotateMaxCaptureBytes+1))
	if err != nil {
		return "", "", err
	}
	if len(content) > differentialLogRotateMaxCaptureBytes {
		return "", "", errors.New("log-rotate archive member exceeds capture limit")
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", "", errors.New("log-rotate archive contains multiple members")
		}
		return "", "", err
	}
	return header.Name, string(content), nil
}

func decodeDifferentialLogRotateState(content string) (differentialLogRotateState, error) {
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var state differentialLogRotateState
	if err := decoder.Decode(&state); err != nil {
		return differentialLogRotateState{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return differentialLogRotateState{}, errors.New("log-rotate observation contains trailing JSON")
	}
	return state, nil
}
