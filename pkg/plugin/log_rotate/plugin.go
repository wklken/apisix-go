package log_rotate

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config

	mu         sync.Mutex
	rotateTime time.Time
	now        func() time.Time
}

const (
	priority = 100
	name     = "log-rotate"

	defaultInterval = 60 * 60
	defaultMaxKept  = 24 * 7
	defaultMaxSize  = -1
	defaultTimeout  = 10000
)

const schema = `
{
  "type": "object",
  "properties": {}
}
`

type Config struct {
	Interval          int    `json:"interval,omitempty"`
	MaxKept           int    `json:"max_kept,omitempty"`
	MaxSize           int64  `json:"max_size,omitempty"`
	Timeout           int    `json:"timeout,omitempty"`
	EnableCompression bool   `json:"enable_compression,omitempty"`
	LogDir            string `json:"log_dir,omitempty"`
	AccessLog         string `json:"access_log,omitempty"`
	ErrorLog          string `json:"error_log,omitempty"`
	EnableAccessLog   *bool  `json:"enable_access_log,omitempty"`
}

type logFile struct {
	path string
	name string
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	p.loadPluginAttr()
	p.applyDefaults()
	if p.now == nil {
		p.now = time.Now
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if err := p.Rotate(p.now()); err != nil {
			logger.Errorf("log-rotate failed: %s", err)
		}
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) Rotate(now time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	files, err := p.filesToRotate(now)
	if err != nil || len(files) == 0 {
		return err
	}

	date := now.Format("2006-01-02_15-04-05")
	for _, file := range files {
		rotated, err := p.rotateFile(file, date)
		if err != nil {
			return err
		}
		if rotated == "" {
			continue
		}
		if p.config.EnableCompression {
			if err := compressFile(rotated); err != nil {
				return err
			}
		}
		if err := p.pruneHistory(file); err != nil {
			return err
		}
	}

	return nil
}

func (p *Plugin) loadPluginAttr() {
	if config.GlobalConfig == nil || config.GlobalConfig.PluginAttr == nil {
		return
	}
	attr, ok := config.GlobalConfig.PluginAttr[name]
	if !ok {
		return
	}
	if p.config.Interval == 0 {
		p.config.Interval = intFromAttr(attr, "interval")
	}
	if p.config.MaxKept == 0 {
		p.config.MaxKept = intFromAttr(attr, "max_kept")
	}
	if p.config.MaxSize == 0 {
		p.config.MaxSize = int64(intFromAttr(attr, "max_size"))
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = intFromAttr(attr, "timeout")
	}
	if value, ok := attr["enable_compression"].(bool); ok {
		p.config.EnableCompression = value
	}
}

func (p *Plugin) applyDefaults() {
	if p.config.Interval == 0 {
		p.config.Interval = defaultInterval
	}
	if p.config.MaxKept == 0 {
		p.config.MaxKept = defaultMaxKept
	}
	if p.config.MaxSize == 0 {
		p.config.MaxSize = defaultMaxSize
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = defaultTimeout
	}
	if p.config.LogDir == "" {
		p.config.LogDir = "logs"
	}
	if p.config.ErrorLog == "" {
		p.config.ErrorLog = filepath.Join(p.config.LogDir, "error.log")
	}
	if p.config.AccessLog == "" {
		p.config.AccessLog = filepath.Join(p.config.LogDir, "access.log")
	}
	if p.config.EnableAccessLog == nil {
		b := true
		p.config.EnableAccessLog = &b
	}
}

func (p *Plugin) filesToRotate(now time.Time) ([]logFile, error) {
	files := p.logFiles()
	if p.shouldRotateByInterval(now) {
		return files, nil
	}
	if p.config.MaxSize < 0 {
		return nil, nil
	}

	rotating := make([]logFile, 0, len(files))
	for _, file := range files {
		size, err := fileSize(file.path)
		if err != nil {
			return nil, err
		}
		if size >= p.config.MaxSize {
			rotating = append(rotating, file)
		}
	}
	return rotating, nil
}

func (p *Plugin) shouldRotateByInterval(now time.Time) bool {
	if p.rotateTime.IsZero() {
		p.rotateTime = nextRotateTime(now, time.Duration(p.config.Interval)*time.Second)
		return false
	}
	if now.Before(p.rotateTime) {
		return false
	}
	for !now.Before(p.rotateTime) {
		p.rotateTime = p.rotateTime.Add(time.Duration(p.config.Interval) * time.Second)
	}
	return true
}

func (p *Plugin) logFiles() []logFile {
	files := []logFile{{path: p.config.ErrorLog, name: filepath.Base(p.config.ErrorLog)}}
	if p.config.EnableAccessLog != nil && *p.config.EnableAccessLog {
		files = append(files, logFile{path: p.config.AccessLog, name: filepath.Base(p.config.AccessLog)})
	}
	return files
}

func (p *Plugin) rotateFile(file logFile, date string) (string, error) {
	if _, err := os.Stat(file.path); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	rotated := filepath.Join(filepath.Dir(file.path), date+"__"+file.name)
	if _, err := os.Stat(rotated); err == nil {
		return rotated, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Rename(file.path, rotated); err != nil {
		return "", err
	}
	return rotated, recreateFile(file.path)
}

func (p *Plugin) pruneHistory(file logFile) error {
	history, err := rotatedHistory(file)
	if err != nil {
		return err
	}
	for i := p.config.MaxKept; i < len(history); i++ {
		if err := os.Remove(history[i]); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func intFromAttr(attr map[string]any, key string) int {
	value, ok := attr[key]
	if !ok {
		return 0
	}
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case uint64:
		return int(v)
	default:
		return 0
	}
}

func nextRotateTime(now time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		return now
	}
	unix := now.Unix()
	seconds := int64(interval / time.Second)
	return time.Unix(unix+seconds-(unix%seconds), 0).In(now.Location())
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return info.Size(), nil
}

func recreateFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	return file.Close()
}

func rotatedHistory(file logFile) ([]string, error) {
	entries, err := os.ReadDir(filepath.Dir(file.path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	suffix := "__" + file.name
	compressedSuffix := suffix + ".tar.gz"
	history := make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		if strings.Contains(name, "__") &&
			(strings.HasSuffix(name, suffix) || strings.HasSuffix(name, compressedSuffix)) {
			history = append(history, filepath.Join(filepath.Dir(file.path), name))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(history)))
	return history, nil
}

func compressFile(path string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(path + ".tar.gz")
	if err != nil {
		return err
	}

	gz := gzip.NewWriter(dst)
	tw := tar.NewWriter(gz)

	info, err := src.Stat()
	if err != nil {
		dst.Close()
		return err
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		dst.Close()
		return err
	}
	header.Name = filepath.Base(path)
	if err := tw.WriteHeader(header); err != nil {
		dst.Close()
		return err
	}
	if _, err := io.Copy(tw, src); err != nil {
		dst.Close()
		return err
	}
	if err := tw.Close(); err != nil {
		dst.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Remove(path)
}
