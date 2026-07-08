package id

import (
	"sync"

	"github.com/gofrs/uuid"
	"github.com/wklken/apisix-go/pkg/config"
)

var (
	generatedOnce sync.Once
	generatedID   string
)

func Get() string {
	if config.GlobalConfig != nil && config.GlobalConfig.Apisix.ID != "" {
		return config.GlobalConfig.Apisix.ID
	}

	generatedOnce.Do(func() {
		generatedID = uuid.Must(uuid.NewV4()).String()
	})

	return generatedID
}
