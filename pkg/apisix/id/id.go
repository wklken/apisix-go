package id

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gofrs/uuid"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
)

var (
	generatedOnce sync.Once
	generatedID   string
	uidFilePath   = filepath.Join("conf", "apisix.uid")
)

func Get() string {
	if config.GlobalConfig != nil && config.GlobalConfig.Apisix.ID != "" {
		return config.GlobalConfig.Apisix.ID
	}

	generatedOnce.Do(func() {
		if content, err := os.ReadFile(uidFilePath); err == nil {
			generatedID = strings.TrimSpace(string(content))
		}
		if generatedID != "" {
			return
		}
		generatedID = uuid.Must(uuid.NewV4()).String()
		if err := os.WriteFile(uidFilePath, []byte(generatedID), 0o600); err != nil {
			logger.Errorf("persist generated apisix id %q: %s", uidFilePath, err)
		}
	})

	return generatedID
}
