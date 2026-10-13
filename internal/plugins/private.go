package plugins

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const privateRecordPrefix = "PLAS1:"

func loadPrivateCipher(dir string) (cipher.AEAD, error) {
	name := filepath.Join(dir, "secret-key")
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		key := make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		file, err = os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return nil, err
		}
		if _, err = file.Write(key); err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, err
		}
		block, err := aes.NewCipher(key)
		clear(key)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCM(block)
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != 32 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("Agent secret key must be a private regular 32-byte file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("Agent secret key belongs to another service UID")
	}
	key := make([]byte, 32)
	if _, err = io.ReadFull(file, key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	clear(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func (e *PrivilegedExecutor) sealPrivate(id string, data []byte) ([]byte, error) {
	if e.privateCipher == nil {
		return nil, errors.New("Agent private encryption is unavailable")
	}
	nonce := make([]byte, e.privateCipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := e.privateCipher.Seal(nonce, nonce, data, []byte("private|"+id))
	return []byte(privateRecordPrefix + base64.RawStdEncoding.EncodeToString(sealed)), nil
}
func (e *PrivilegedExecutor) openPrivate(id string, data []byte) ([]byte, bool, error) {
	if !strings.HasPrefix(string(data), privateRecordPrefix) {
		return data, true, nil
	}
	if e.privateCipher == nil {
		return nil, false, errors.New("Agent private encryption is unavailable")
	}
	sealed, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(string(data), privateRecordPrefix))
	if err != nil || len(sealed) < e.privateCipher.NonceSize()+e.privateCipher.Overhead() {
		return nil, false, failure("private_record_invalid", "Protected Agent record is incomplete or invalid", 412)
	}
	nonce := sealed[:e.privateCipher.NonceSize()]
	opened, err := e.privateCipher.Open(nil, nonce, sealed[e.privateCipher.NonceSize():], []byte("private|"+id))
	if err != nil {
		return nil, false, failure("private_record_invalid", "Protected Agent record authentication failed", 412)
	}
	return opened, false, nil
}
