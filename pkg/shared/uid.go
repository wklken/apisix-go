package shared

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
)

type ConfigUID struct {
	// string buffer to add parts of the uid
	buffer bytes.Buffer
}

func NewConfigUID() *ConfigUID {
	return &ConfigUID{}
}

func (uid *ConfigUID) Add(parts ...any) {
	for _, part := range parts {
		s := fmt.Sprintf("%s", part)
		uid.buffer.WriteString(s)
		uid.buffer.WriteString(":")
	}
}

func (uid *ConfigUID) String() string {
	id := uid.buffer.Bytes()
	hash := md5.Sum(id)
	return hex.EncodeToString(hash[:])
}
