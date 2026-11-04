// Package vault stores notification credentials outside application snapshots.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

type Config struct {
	BarkURL        string `json:"bark_url"`
	BarkKey        string `json:"bark_key"`
	RainCookie     string `json:"rain_cookie,omitempty"`
	ZhidaoSession  string `json:"zhidao_session,omitempty"`
	ZhidaoPhone    string `json:"zhidao_phone,omitempty"`
	ZhidaoPassword string `json:"zhidao_password,omitempty"`
	AnalysisAPIKey string `json:"analysis_api_key,omitempty"`
	AnalysisModel  string `json:"analysis_model,omitempty"`
}

type Vault struct {
	mu     sync.Mutex
	path   string
	aead   cipher.AEAD
	config Config
}

func Open(dir string) (*Vault, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, "master.key")
	encPath := filepath.Join(dir, "secrets.enc")
	key, err := os.ReadFile(keyPath)
	if os.IsNotExist(err) {
		if key, err = createKey(keyPath); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		// A crash or full disk during first-run creation can leave a short key.
		// Regenerate only when no ciphertext exists; replacing a key that may
		// have encrypted credentials would destroy them.
		if _, statErr := os.Stat(encPath); statErr == nil {
			return nil, errors.New("credential key has invalid length")
		} else if !os.IsNotExist(statErr) {
			return nil, statErr
		}
		if key, err = createKey(keyPath); err != nil {
			return nil, err
		}
	}
	// Existing installations may have been created with a permissive umask or
	// copied from another host. Credentials are bearer secrets, so normalize the
	// key file mode before using it instead of relying on the original mode.
	if err := os.Chmod(keyPath, 0600); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	v := &Vault{path: encPath, aead: aead}
	data, err := os.ReadFile(v.path)
	if os.IsNotExist(err) {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(v.path, 0600); err != nil {
		return nil, err
	}
	if len(data) < aead.NonceSize() {
		return nil, errors.New("credential store is truncated")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte("DueRadar/v1"))
	if err != nil {
		return nil, errors.New("credential store authentication failed")
	}
	if err = json.Unmarshal(plain, &v.config); err != nil {
		return nil, err
	}
	return v, nil
}

// createKey generates a 32-byte key and commits it atomically, so a crash
// cannot leave a partially written key that would make the vault unusable.
func createKey(path string) ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".dueradar-key-*.tmp")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	defer os.Remove(name)
	_, err = f.Write(key)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err = os.Rename(name, path); err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		if d, openErr := os.Open(dir); openErr == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	return key, nil
}

func (v *Vault) Get() Config { v.mu.Lock(); defer v.mu.Unlock(); return v.config }

func (v *Vault) Set(config Config) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.write(config)
}

// Update merges independent configuration changes in one transaction.
func (v *Vault) Update(change func(*Config) error) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	config := v.config
	if err := change(&config); err != nil {
		return err
	}
	return v.write(config)
}

func (v *Vault) write(config Config) error {
	plain, err := json.Marshal(config)
	if err != nil {
		return err
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	data := v.aead.Seal(nonce, nonce, plain, []byte("DueRadar/v1"))
	dir := filepath.Dir(v.path)
	f, err := os.CreateTemp(dir, ".dueradar-secrets-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, v.path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if d, openErr := os.Open(dir); openErr == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	v.config = config
	return nil
}
