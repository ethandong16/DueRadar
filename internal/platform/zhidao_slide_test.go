package platform

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

// decryptAccountPayload mirrors what the login center does: base64 decode,
// AES-CBC decrypt with the published key and IV, then strip PKCS#7.
func decryptAccountPayload(t *testing.T, encoded string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		t.Fatalf("ciphertext length %d is not a block multiple", len(raw))
	}
	block, err := aes.NewCipher([]byte(zhidaoLoginKey))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, []byte(zhidaoLoginIV)).CryptBlocks(out, raw)
	padding := int(out[len(out)-1])
	if padding == 0 || padding > aes.BlockSize {
		t.Fatalf("invalid padding %d", padding)
	}
	if !bytes.Equal(out[len(out)-padding:], bytes.Repeat([]byte{byte(padding)}, padding)) {
		t.Fatal("padding bytes do not match the declared length")
	}
	return out[:len(out)-padding]
}

func TestEncryptAccountPayloadRoundTrips(t *testing.T) {
	// The payload has Chinese characters and a length that is not a block multiple.
	payload, err := json.Marshal(map[string]string{
		"account":  "13800000000",
		"password": "secret-password",
		"validate": "captcha-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := encryptAccountPayload(payload)
	if encoded == "" {
		t.Fatal("encryption returned an empty body")
	}
	if got := decryptAccountPayload(t, encoded); !bytes.Equal(got, payload) {
		t.Fatalf("round trip mismatch: %s", got)
	}
}

func TestLoginCredentialsRequiresOneTimePassword(t *testing.T) {
	// The login center redeems rt.pwd at passport; success without it cannot
	// produce a session, so it must not be treated as a completed login.
	var withRT accountLoginResult
	if err := json.Unmarshal([]byte(`{"status":1,"rt":{"pwd":"once-1","uuid":"u-1"}}`), &withRT); err != nil {
		t.Fatal(err)
	}
	pwd, uuid, err := loginCredentials(&withRT)
	if err != nil || pwd != "once-1" || uuid != "u-1" {
		t.Fatalf("got %q %q %v", pwd, uuid, err)
	}

	var withoutRT accountLoginResult
	if err := json.Unmarshal([]byte(`{"status":1,"rt":{}}`), &withoutRT); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loginCredentials(&withoutRT); !errors.Is(err, ErrLoginIncomplete) {
		t.Fatalf("missing pwd returned %v", err)
	}

	var rejected accountLoginResult
	if err := json.Unmarshal([]byte(`{"status":-4}`), &rejected); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loginCredentials(&rejected); !errors.Is(err, ErrCaptchaRejected) {
		t.Fatalf("captcha rejection returned %v", err)
	}
}
