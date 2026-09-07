package config

import (
	"path/filepath"
	"testing"
)

func TestShippedEncryptionDefaultsMatchAPISIX(t *testing.T) {
	path, err := filepath.Abs("../../conf/config-default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	effective, err := LoadEffective(LoadRequest{DefaultPath: path, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	encryption := effective.Config.Apisix.DataEncryption
	if !encryption.EnableEncryptFields || len(encryption.Keyring) != 2 || encryption.Keyring[0] != "qeddd145sfvddff3" ||
		encryption.Keyring[1] != "edd1c9f0985e76a2" {
		t.Fatal("shipped encryption defaults differ from APISIX 3.17")
	}
}
