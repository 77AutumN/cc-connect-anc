package config

import (
	"github.com/BurntSushi/toml"
	"testing"
)

func TestFileWorkDefaultOffAndExplicitConfiguration(t *testing.T) {
	var legacy, configured ProjectConfig
	if _, err := toml.Decode(`name="fictional"`, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.FileWork.Enabled {
		t.Fatal("legacy project enabled file host")
	}
	if _, err := toml.Decode("name='fictional'\n[file_work]\nenabled=true\nledger='/protected/fixture.db'\n", &configured); err != nil {
		t.Fatal(err)
	}
	if !configured.FileWork.Enabled || configured.FileWork.Ledger != "/protected/fixture.db" {
		t.Fatal("explicit configuration lost")
	}
}
