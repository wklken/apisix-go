package compiler

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/wklken/apisix-go/pkg/plugin"
)

const pluginTaskOwnerFactoryMaxLen = 48

var errPluginTaskOwnerIdentity = errors.New("plugin task owner identity is invalid")

func pluginTaskOwnerPrefix(instance plugin.InstanceKey) (string, error) {
	canonical, err := canonicalPluginTaskOwnerIdentity(instance)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return "plugin/" + sanitizePluginTaskOwnerFactory(instance.Factory) + "/" +
		hex.EncodeToString(digest[:]), nil
}

func canonicalPluginTaskOwnerIdentity(instance plugin.InstanceKey) ([]byte, error) {
	if strings.TrimSpace(instance.Factory) == "" ||
		instance.Attempt == ([32]byte{}) ||
		instance.Scope < plugin.ScopeSystem || instance.Scope > plugin.ScopeConsumer ||
		instance.Owner.Kind == "" || instance.Owner.ID == "" ||
		instance.ConfigDigest == ([32]byte{}) {
		return nil, fmt.Errorf("%w: incomplete instance key", errPluginTaskOwnerIdentity)
	}

	var canonical bytes.Buffer
	if err := appendPluginTaskOwnerString(&canonical, "apisix-go/plugin-task-owner/v1"); err != nil {
		return nil, err
	}
	if err := appendPluginTaskOwnerString(&canonical, instance.Factory); err != nil {
		return nil, err
	}
	canonical.Write(instance.Attempt[:])
	canonical.WriteByte(byte(instance.Scope))
	if err := appendPluginTaskOwnerString(&canonical, string(instance.Owner.Kind)); err != nil {
		return nil, err
	}
	if err := appendPluginTaskOwnerString(&canonical, instance.Owner.ID); err != nil {
		return nil, err
	}
	canonical.Write(instance.ConfigDigest[:])
	return canonical.Bytes(), nil
}

func appendPluginTaskOwnerString(buffer *bytes.Buffer, value string) error {
	if uint64(len(value)) > math.MaxUint32 {
		return fmt.Errorf("%w: string length exceeds uint32", errPluginTaskOwnerIdentity)
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	buffer.Write(length[:])
	buffer.WriteString(value)
	return nil
}

func sanitizePluginTaskOwnerFactory(factory string) string {
	sanitized := make([]byte, 0, min(len(factory), pluginTaskOwnerFactoryMaxLen))
	disallowed := false
	for index := range len(factory) {
		value := factory[index]
		switch {
		case value >= 'A' && value <= 'Z':
			sanitized = append(sanitized, value+('a'-'A'))
			disallowed = false
		case value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '-':
			sanitized = append(sanitized, value)
			disallowed = false
		default:
			if !disallowed {
				sanitized = append(sanitized, '-')
			}
			disallowed = true
		}
	}
	result := strings.Trim(string(sanitized), "-")
	if len(result) > pluginTaskOwnerFactoryMaxLen {
		result = result[:pluginTaskOwnerFactoryMaxLen]
	}
	result = strings.TrimRight(result, "-")
	if result == "" {
		return "unknown"
	}
	return result
}
