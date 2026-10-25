package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
)

// Sealer encrypts what vigil must keep itself, the secret half of its own
// login to the store, with a key in a file of its own (AES-256-GCM). The
// database then holds only ciphertext: a copy of it, or of a backup,
// reveals nothing without the key file.
type Sealer struct{ aead cipher.AEAD }

// OpenSealer reads the key file, making it (mode 0600) when it is missing.
func OpenSealer(path string) (*Sealer, error) {
	key, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, ferr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if ferr != nil {
			return nil, fmt.Errorf("making the key file: %w", ferr)
		}
		_, err = f.Write(key)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		return nil, fmt.Errorf("the key file %s: %w", path, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("the key file %s does not hold a 32-byte key", path)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead}, nil
}

// Seal encrypts; what is bound to label (where the value is kept) cannot
// be moved to another place and still open.
func (s *Sealer) Seal(label, plain string) string {
	nonce := make([]byte, s.aead.NonceSize())
	rand.Read(nonce)
	return base64.StdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(plain), []byte(label)))
}

func (s *Sealer) Open(label, sealed string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(b) < s.aead.NonceSize() {
		return "", errors.New("not a sealed value")
	}
	plain, err := s.aead.Open(nil, b[:s.aead.NonceSize()], b[s.aead.NonceSize():], []byte(label))
	if err != nil {
		return "", errors.New("the sealed value does not open with this key file")
	}
	return string(plain), nil
}
