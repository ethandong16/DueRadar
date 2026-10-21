package vault

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
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

func TestIndependentCredentialUpdatesPreserved(t *testing.T) {
	v, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := v.Update(func(c *Config) error { c.BarkKey = "bark-test"; return nil }); err != nil {
			t.Error(err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := v.Update(func(c *Config) error { c.RainCookie = "sessionid=test"; return nil }); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	got := v.Get()
	if got.BarkKey != "bark-test" || got.RainCookie != "sessionid=test" {
		t.Fatal("concurrent update lost unrelated credentials")
	}
}

func TestFailedRenamePreservesCredentialsAndCleansTransaction(t *testing.T) {
	dir := t.TempDir()
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{BarkKey: "original-test-key", RainCookie: "sessionid=original"}
	if err = v.Set(want); err != nil {
		t.Fatal(err)
	}
	storedPath := v.path
	before, err := os.ReadFile(storedPath)
	if err != nil {
		t.Fatal(err)
	}
	// A directory at the commit destination makes Rename fail on every OS,
	// without relying on privilege-dependent file permissions.
	v.path = filepath.Join(dir, "blocked")
	if err = os.Mkdir(v.path, 0700); err != nil {
		t.Fatal(err)
	}
	if err = v.Set(Config{BarkKey: "replacement-test-key"}); err == nil {
		t.Fatal("commit to a directory unexpectedly succeeded")
	}
	if v.Get() != want {
		t.Error("failed commit changed the in-memory credentials")
	}
	after, err := os.ReadFile(storedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("failed commit changed the stored ciphertext")
	}
	reopened, err := Open(dir)
	if err != nil || reopened.Get() != want {
		t.Fatal("failed commit prevented credential recovery", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 { // master.key, secrets.enc, and the blocked directory.
		t.Errorf("failed commit left temporary files: got %d directory entries, want 3", len(entries))
	}
}

func TestStaleFixedTemporaryPathDoesNotBlockSave(t *testing.T) {
	dir := t.TempDir()
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stalePath := v.path + ".tmp"
	if err = os.Mkdir(stalePath, 0700); err != nil {
		t.Fatal(err)
	}
	want := Config{ZhidaoSession: "session=test"}
	if err = v.Set(want); err != nil {
		t.Fatal("stale fixed temporary path blocked saving credentials")
	}
	reopened, err := Open(dir)
	if err != nil || reopened.Get() != want {
		t.Fatal("saved credentials did not survive reopening", err)
	}
	info, err := os.Stat(stalePath)
	if err != nil || !info.IsDir() {
		t.Fatal("saving changed the unrelated stale path", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 { // master.key, secrets.enc, and the stale directory.
		t.Errorf("successful commit left temporary files: got %d directory entries, want 3", len(entries))
	}
}
