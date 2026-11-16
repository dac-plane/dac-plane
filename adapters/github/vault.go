package githubadapter

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var errAbsent = errors.New("record not found")
var bucket = []byte("sealed")

// Vault encrypts every stored value with AES-256-GCM, binding the ciphertext
// to its record key. bbolt supplies exclusive process ownership and durable
// transactions; the file is private to the adapter's operating-system user.
type Vault struct {
	db  *bolt.DB
	box cipher.AEAD
}

func OpenVault(path string, key []byte) (*Vault, error) {
	if len(key) != 32 {
		return nil, errors.New("vault key must be exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	box, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, errors.New("cannot open connector vault")
	}
	if err := os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error { _, err := tx.CreateBucketIfNotExists(bucket); return err }); err != nil {
		db.Close()
		return nil, err
	}
	return &Vault{db: db, box: box}, nil
}
func (v *Vault) Close() error { return v.db.Close() }
func (v *Vault) read(tx *bolt.Tx, key string, out any) error {
	raw := tx.Bucket(bucket).Get([]byte(key))
	if raw == nil {
		return errAbsent
	}
	n := v.box.NonceSize()
	if len(raw) < n {
		return errors.New("invalid sealed record")
	}
	plain, err := v.box.Open(nil, raw[:n], raw[n:], []byte(key))
	if err != nil {
		return errors.New("cannot unseal connector record")
	}
	if err := json.Unmarshal(plain, out); err != nil {
		return errors.New("invalid connector record")
	}
	return nil
}
func (v *Vault) write(tx *bolt.Tx, key string, value any) error {
	plain, err := json.Marshal(value)
	if err != nil {
		return err
	}
	nonce := make([]byte, v.box.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	return tx.Bucket(bucket).Put([]byte(key), v.box.Seal(nonce, nonce, plain, []byte(key)))
}
func (v *Vault) load(key string, out any) error {
	return v.db.View(func(tx *bolt.Tx) error { return v.read(tx, key, out) })
}
func (v *Vault) save(key string, value any) error {
	return v.db.Update(func(tx *bolt.Tx) error { return v.write(tx, key, value) })
}
