package shared

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strconv"
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
		uid.buffer.WriteString(strconv.Itoa(len(s)))
		uid.buffer.WriteByte(':')
		uid.buffer.WriteString(s)
	}
}

func (uid *ConfigUID) String() string {
	id := uid.buffer.Bytes()
	hash := md5.Sum(id)
	return hex.EncodeToString(hash[:])
}
