package config

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestExampleConfigs ensures every shipped example in configs/ parses and
// validates once the REPLACE_ME placeholders are substituted with real keys.
func TestExampleConfigs(t *testing.T) {
	files, err := filepath.Glob("../../configs/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no example configs found: %v", err)
	}
	re := regexp.MustCompile(`REPLACE_ME[A-Z0-9_]*`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		b = re.ReplaceAllFunc(b, func([]byte) []byte {
			k := make([]byte, 32)
			rand.Read(k)
			return []byte(base64.StdEncoding.EncodeToString(k))
		})
		c, err := Parse(b)
		if err != nil {
			t.Errorf("%s: parse: %v", filepath.Base(f), err)
			continue
		}
		if err := c.Validate(); err != nil {
			t.Errorf("%s: validate: %v", filepath.Base(f), err)
		}
	}
}
