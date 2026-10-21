package vault

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptedRoundTripAndTampering(t *testing.T) {
	dir := t.TempDir()
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{BarkURL: "https://api.day.app", BarkKey: "secret-do-not-print"}
	if err = v.Set(want); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "secrets.enc"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(want.BarkKey)) {
		t.Fatal("plaintext secret")
	}
	reopened, err := Open(dir)
	if err != nil || reopened.Get() != want {
		t.Fatal("round trip failed", err)
	}
	data[len(data)-1] ^= 1
	if err = os.WriteFile(filepath.Join(dir, "secrets.enc"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(dir); err == nil {
		t.Fatal("tampered credentials accepted")
	}
}
