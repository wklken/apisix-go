package pluginintegration

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	differentialOracleFileDirectory = "/tmp/apisix-go-differential-files"
	differentialFileMaxCaptureBytes = 1 << 20
)

type differentialFileSides struct {
	CandidatePath    string
	OracleHostDir    string
	OracleHostPath   string
	OracleConfigPath string
}

func validateDifferentialFileCapture(capture DifferentialFileCapture) error {
	if capture.Name == "" || filepath.Base(capture.Name) != capture.Name || capture.Name == "." {
		return fmt.Errorf("differential file name %q must be a base name", capture.Name)
	}
	if capture.MaxBytes < 1 || capture.MaxBytes > differentialFileMaxCaptureBytes {
		return fmt.Errorf(
			"differential file max_bytes = %d, want 1..%d",
			capture.MaxBytes,
			differentialFileMaxCaptureBytes,
		)
	}
	if capture.WaitTimeoutMillis < 1 || capture.WaitTimeoutMillis > 5000 {
		return fmt.Errorf("differential file wait_timeout_millis = %d, want 1..5000", capture.WaitTimeoutMillis)
	}
	if capture.ExpectedLines < 0 || capture.ExpectedLines > 1000 {
		return fmt.Errorf("differential file expected_lines = %d, want 0..1000", capture.ExpectedLines)
	}
	return nil
}

func prepareDifferentialFileSides(workDir string, capture DifferentialFileCapture) (differentialFileSides, error) {
	if err := validateDifferentialFileCapture(capture); err != nil {
		return differentialFileSides{}, err
	}
	candidateDir := filepath.Join(workDir, "candidate-files")
	oracleHostDir := filepath.Join(workDir, "oracle-files")
	if err := os.MkdirAll(candidateDir, 0o700); err != nil {
		return differentialFileSides{}, fmt.Errorf("create candidate file directory: %w", err)
	}
	if err := os.MkdirAll(oracleHostDir, 0o777); err != nil {
		return differentialFileSides{}, fmt.Errorf("create oracle file directory: %w", err)
	}
	if err := os.Chmod(oracleHostDir, 0o777); err != nil {
		return differentialFileSides{}, fmt.Errorf("make oracle file directory container-writable: %w", err)
	}
	return differentialFileSides{
		CandidatePath:    filepath.Join(candidateDir, capture.Name),
		OracleHostDir:    oracleHostDir,
		OracleHostPath:   filepath.Join(oracleHostDir, capture.Name),
		OracleConfigPath: filepath.Join(differentialOracleFileDirectory, capture.Name),
	}, nil
}

func projectDifferentialSideFile(config map[string]any, path string) (map[string]any, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("differential side file path %q is not absolute", path)
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("marshal differential side file config: %w", err)
	}
	var projected map[string]any
	if err := yaml.Unmarshal(data, &projected); err != nil {
		return nil, fmt.Errorf("clone differential side file config: %w", err)
	}
	replaced := replaceDifferentialSideFilePlaceholder(projected, path)
	if replaced != 1 {
		return nil, fmt.Errorf("differential side file placeholder count = %d, want 1", replaced)
	}
	return projected, nil
}

func replaceDifferentialSideFilePlaceholder(value any, path string) int {
	replaced := 0
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if text, ok := child.(string); ok && text == differentialSideFilePlaceholder {
				typed[key] = path
				replaced++
				continue
			}
			replaced += replaceDifferentialSideFilePlaceholder(child, path)
		}
	case []any:
		for index, child := range typed {
			if text, ok := child.(string); ok && text == differentialSideFilePlaceholder {
				typed[index] = path
				replaced++
				continue
			}
			replaced += replaceDifferentialSideFilePlaceholder(child, path)
		}
	}
	return replaced
}

func collectDifferentialFileObservation(
	path string,
	capture DifferentialFileCapture,
) (DifferentialFileObservation, error) {
	if err := validateDifferentialFileCapture(capture); err != nil {
		return DifferentialFileObservation{}, err
	}
	deadline := time.Now().Add(time.Duration(capture.WaitTimeoutMillis) * time.Millisecond)
	var last DifferentialFileObservation
	for {
		observation, err := snapshotDifferentialFile(path, capture)
		if err != nil {
			return DifferentialFileObservation{}, err
		}
		last = observation
		if observation.Exists &&
			(capture.ExpectedLines == 0 || strings.Count(observation.Content, "\n") >= capture.ExpectedLines) {
			return observation, nil
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf(
				"wait for differential file %s: got %d/%d complete lines",
				capture.Name,
				strings.Count(last.Content, "\n"),
				capture.ExpectedLines,
			)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func snapshotDifferentialFile(path string, capture DifferentialFileCapture) (DifferentialFileObservation, error) {
	observation := DifferentialFileObservation{Name: capture.Name}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return observation, nil
	}
	if err != nil {
		return DifferentialFileObservation{}, fmt.Errorf("open differential file %s: %w", capture.Name, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return DifferentialFileObservation{}, fmt.Errorf("stat differential file %s: %w", capture.Name, err)
	}
	content, err := io.ReadAll(io.LimitReader(file, capture.MaxBytes+1))
	if err != nil {
		return DifferentialFileObservation{}, fmt.Errorf("read differential file %s: %w", capture.Name, err)
	}
	observation.Exists = true
	observation.Size = info.Size()
	observation.Truncated = int64(len(content)) > capture.MaxBytes || info.Size() > capture.MaxBytes
	if int64(len(content)) > capture.MaxBytes {
		content = content[:capture.MaxBytes]
	}
	observation.Content = string(content)
	return observation, nil
}

func differentialOracleFileVolume(hostDir string) string {
	if hostDir == "" {
		return ""
	}
	return hostDir + ":" + differentialOracleFileDirectory + ":rw"
}

func decodeDifferentialJSONLine(content string) (map[string]any, error) {
	if !strings.HasSuffix(content, "\n") {
		return nil, errors.New("JSONL entry is missing its line terminator")
	}
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if len(lines) != 1 || strings.TrimSpace(lines[0]) == "" {
		return nil, fmt.Errorf("JSONL entry count = %d, want 1", len(lines))
	}
	decoder := json.NewDecoder(strings.NewReader(lines[0]))
	decoder.UseNumber()
	var entry map[string]any
	if err := decoder.Decode(&entry); err != nil {
		return nil, fmt.Errorf("decode JSONL entry: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("JSONL entry contains trailing JSON")
	}
	return entry, nil
}
