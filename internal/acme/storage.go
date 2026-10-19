// Package acme implements the daemon-managed ACME certificate client
// specified in docs/acme-certificates-spec.md: account + certificate
// persistence under <cache.dir>/acme, HTTP-01 challenge serving, and a
// renewal loop with hot reload. The ACME certificate is served on the
// surfaces that select it (cache.listen_tls, admin.listen); it is never
// used as a MITM CA.
package acme

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Storage layout under StorageDir (default <cache.dir>/acme):
//
//	account.key    account private key (ECDSA P-256), PEM, 0600
//	account.json   account URL + directory + dns_name metadata, 0644
//	cert.pem       full chain PEM (leaf first), 0644
//	privkey.pem    certificate private key, PEM, 0600
//	cert.json      directory_url + dns_name the certificate was issued for, 0644
//	.acme.lock     interprocess flock serializing issuance/renewal
const (
	accountKeyFile  = "account.key"
	accountMetaFile = "account.json"
	certChainFile   = "cert.pem"
	certKeyFile     = "privkey.pem"
	certMetaFile    = "cert.json"
	lockFile        = ".acme.lock"
)

// accountMeta is the persisted account metadata. The account URL is the
// RFC 8555 "kid" reused across restarts so the account is never
// re-created.
type accountMeta struct {
	AccountURL   string `json:"account_url"`
	DirectoryURL string `json:"directory_url"`
	DNSName      string `json:"dns_name"`
}

// certMeta records where the stored certificate comes from, so a
// certificate issued by another directory (e.g. Let's Encrypt staging
// before a switch to production) is never served.
type certMeta struct {
	DirectoryURL string `json:"directory_url"`
	DNSName      string `json:"dns_name"`
}

// ensurePrivateDir creates dir with 0700 (or tightens an existing dir to
// 0700), matching the tlsmitm CA storage posture.
func ensurePrivateDir(dir string) error {
	if dir == "" {
		return errors.New("acme: empty storage dir")
	}
	info, err := os.Stat(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("acme: stat %s: %w", dir, err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("acme: create %s: %w", dir, err)
		}
		info, err = os.Stat(dir)
		if err != nil {
			return fmt.Errorf("acme: stat %s after create: %w", dir, err)
		}
	}
	if !info.IsDir() {
		return fmt.Errorf("acme: %s exists but is not a directory", dir)
	}
	if info.Mode().Perm() == 0o700 {
		return nil
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("acme: chmod %s 0700: %w", dir, err)
	}
	return nil
}

// writeFileAtomic writes data to <dir>/<name> via a temp file + rename so
// a crash mid-write never leaves a truncated key or certificate. Private
// material gets 0600; public material 0644.
func writeFileAtomic(dir, name string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(dir, name+".tmp")
	if err != nil {
		return fmt.Errorf("acme: create temp for %s: %w", name, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("acme: write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("acme: fsync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("acme: close %s: %w", name, err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("acme: chmod %s: %w", name, err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("acme: rename %s: %w", name, err)
	}
	return nil
}

// generateAccountKey creates the ECDSA P-256 account key.
func generateAccountKey() (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("acme: generate account key: %w", err)
	}
	return key, nil
}

// encodePrivateKeyPEM marshals an ECDSA private key as PKCS#8 PEM.
func encodePrivateKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("acme: marshal private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// loadAccount reads the stored account key + metadata. ok=false when
// either file is absent (first boot).
func loadAccount(dir string) (key *ecdsa.PrivateKey, meta accountMeta, ok bool, err error) {
	keyDER, err := os.ReadFile(filepath.Join(dir, accountKeyFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, accountMeta{}, false, nil
		}
		return nil, accountMeta{}, false, fmt.Errorf("acme: read %s: %w", accountKeyFile, err)
	}
	key, err = parsePrivateKeyPEM(keyDER)
	if err != nil {
		return nil, accountMeta{}, false, err
	}
	metaRaw, err := os.ReadFile(filepath.Join(dir, accountMetaFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, accountMeta{}, false, nil
		}
		return nil, accountMeta{}, false, fmt.Errorf("acme: read %s: %w", accountMetaFile, err)
	}
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return nil, accountMeta{}, false, fmt.Errorf("acme: parse %s: %w", accountMetaFile, err)
	}
	if meta.AccountURL == "" {
		return nil, accountMeta{}, false, fmt.Errorf("acme: %s has no account_url", accountMetaFile)
	}
	return key, meta, true, nil
}

func parsePrivateKeyPEM(raw []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("acme: account key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("acme: parse account key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("acme: account key is not ECDSA")
	}
	return key, nil
}

// saveAccount persists the account key (0600) and metadata (0644)
// atomically.
func saveAccount(dir string, key *ecdsa.PrivateKey, meta accountMeta) error {
	keyPEM, err := encodePrivateKeyPEM(key)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(dir, accountKeyFile, keyPEM, 0o600); err != nil {
		return err
	}
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("acme: marshal %s: %w", accountMetaFile, err)
	}
	return writeFileAtomic(dir, accountMetaFile, metaRaw, 0o644)
}

// errStoredCertInvalid marks a stored certificate that cannot be used:
// unparseable, or a chain and key that do not match (a crash between the
// two renames in saveCert, or a reader racing a writer). Callers treat it
// as "no certificate" and re-issue rather than refusing to start.
var errStoredCertInvalid = errors.New("acme: stored certificate is invalid")

// loadCert reads the stored certificate chain + key and its metadata.
// ok=false when the chain or key is absent. A missing cert.json yields a
// zero certMeta (origin unknown). Invalid material wraps
// errStoredCertInvalid; other errors are I/O failures.
func loadCert(dir string) (*tls.Certificate, certMeta, bool, error) {
	chainPEM, err := os.ReadFile(filepath.Join(dir, certChainFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, certMeta{}, false, nil
	} else if err != nil {
		return nil, certMeta{}, false, fmt.Errorf("acme: read %s: %w", certChainFile, err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, certKeyFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, certMeta{}, false, nil
	} else if err != nil {
		return nil, certMeta{}, false, fmt.Errorf("acme: read %s: %w", certKeyFile, err)
	}
	cert, err := tls.X509KeyPair(chainPEM, keyPEM)
	if err != nil {
		return nil, certMeta{}, false, fmt.Errorf("%w: %v", errStoredCertInvalid, err)
	}
	var meta certMeta
	metaRaw, err := os.ReadFile(filepath.Join(dir, certMetaFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, certMeta{}, false, fmt.Errorf("acme: read %s: %w", certMetaFile, err)
	default:
		if err := json.Unmarshal(metaRaw, &meta); err != nil {
			return nil, certMeta{}, false, fmt.Errorf("%w: parse %s: %v", errStoredCertInvalid, certMetaFile, err)
		}
	}
	return &cert, meta, true, nil
}

// saveCert persists the certificate private key (0600), the chain (leaf
// first, 0644) and then its metadata (0644), each atomically. The
// metadata goes last: a crash before it leaves a certificate whose origin
// does not match, which is re-issued rather than trusted.
func saveCert(dir string, cert *tls.Certificate, meta certMeta) error {
	var chainPEM []byte
	for _, der := range cert.Certificate {
		chainPEM = append(chainPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	if len(chainPEM) == 0 {
		return errors.New("acme: certificate has no chain to persist")
	}
	key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return fmt.Errorf("acme: certificate key is %T, want ECDSA", cert.PrivateKey)
	}
	keyPEM, err := encodePrivateKeyPEM(key)
	if err != nil {
		return err
	}
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("acme: marshal %s: %w", certMetaFile, err)
	}
	if err := writeFileAtomic(dir, certKeyFile, keyPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(dir, certChainFile, chainPEM, 0o644); err != nil {
		return err
	}
	return writeFileAtomic(dir, certMetaFile, metaRaw, 0o644)
}
