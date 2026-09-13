package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
)

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func tokenHash(s string) string { h := sha256.Sum256([]byte(s)); return fmt.Sprintf("%x", h) }

func loadKey(path, dbpath string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if info, e := os.Stat(dbpath); e == nil && info.Size() > 0 {
			return nil, fmt.Errorf("chave ausente: restaure a chave original antes de abrir o banco existente")
		}
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		b = make([]byte, 32)
		if _, err = rand.Read(b); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		_, err = f.Write(b)
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("chave mestra inválida")
	}
	return b, nil
}

func encrypt(key []byte, s string) (string, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	a, err := cipher.NewGCM(b)
	if err != nil {
		return "", err
	}
	n := make([]byte, a.NonceSize())
	if _, err = rand.Read(n); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(a.Seal(n, n, []byte(s), []byte("baseguard-credential-v1"))), nil
}
func decrypt(key []byte, s string) (string, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	a, err := cipher.NewGCM(b)
	if err != nil {
		return "", err
	}
	p, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(p) < a.NonceSize() {
		return "", fmt.Errorf("credencial inválida")
	}
	p, err = a.Open(nil, p[:a.NonceSize()], p[a.NonceSize():], []byte("baseguard-credential-v1"))
	return string(p), err
}
