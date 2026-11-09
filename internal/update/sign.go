package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Release signatures bind a tag to the exact SHA256SUMS published with it.
// The private key stays with the release owner, outside CI; a node installs a
// release only when SHA256SUMS.sig verifies with a key compiled into the
// running binary. Signing the tag prevents an old signed SHA256SUMS from being
// republished under a newer tag.
const (
	publicKeyPrefix  = "rmc-release-ed25519:"
	privateKeyPrefix = "rmc-release-ed25519-private:"
	signatureHeader  = "rmc-release-signature v1"
	maxSignature     = 4 << 10
)

// PublicKey is a trusted release-signing key.
type PublicKey struct {
	ID  string
	Key ed25519.PublicKey
}

func keyID(k ed25519.PublicKey) string {
	d := sha256.Sum256(k)
	return hex.EncodeToString(d[:])[:16]
}

// ParsePublicKey reads "rmc-release-ed25519:<base64>".
func ParsePublicKey(s string) (PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), publicKeyPrefix))
	if !strings.HasPrefix(strings.TrimSpace(s), publicKeyPrefix) || err != nil || len(b) != ed25519.PublicKeySize {
		return PublicKey{}, errors.New("invalid release public key")
	}
	k := ed25519.PublicKey(b)
	return PublicKey{ID: keyID(k), Key: k}, nil
}

// ParsePrivateKey reads the private key file content written by GenerateKey.
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	s = strings.TrimSpace(s)
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, privateKeyPrefix))
	if !strings.HasPrefix(s, privateKeyPrefix) || err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid release private key")
	}
	return ed25519.PrivateKey(b), nil
}

// GenerateKey returns a new public key line and private key file content.
func GenerateKey() (public, private string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return publicKeyPrefix + base64.StdEncoding.EncodeToString(pub), privateKeyPrefix + base64.StdEncoding.EncodeToString(priv) + "\n", nil
}

func signedMessage(tag string, sums []byte) []byte {
	d := sha256.Sum256(sums)
	return []byte(signatureHeader + "\ntag: " + tag + "\nsha256sums: " + hex.EncodeToString(d[:]) + "\n")
}

func validTag(tag string) error {
	if !strings.HasPrefix(tag, "v") {
		return errors.New("release tag must start with v")
	}
	_, err := ParseVersion(strings.TrimPrefix(tag, "v"))
	return err
}

// Sign produces the content of SHA256SUMS.sig for a release tag.
func Sign(priv ed25519.PrivateKey, tag string, sums []byte) ([]byte, error) {
	if err := validTag(tag); err != nil {
		return nil, err
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("invalid release private key")
	}
	sig := ed25519.Sign(priv, signedMessage(tag, sums))
	return fmt.Appendf(nil, "%s\nkey: %s\nsignature: %s\n", signatureHeader, keyID(pub), base64.StdEncoding.EncodeToString(sig)), nil
}

// Verify checks SHA256SUMS.sig for tag against the trusted keys and returns
// the ID of the key that signed it.
func Verify(keys []PublicKey, tag string, sums, signature []byte) (string, error) {
	if err := validTag(tag); err != nil {
		return "", err
	}
	if len(signature) > maxSignature {
		return "", errors.New("release signature is too large")
	}
	lines := strings.Split(strings.TrimRight(string(bytes.ReplaceAll(signature, []byte("\r\n"), []byte("\n"))), "\n"), "\n")
	if len(lines) != 3 || lines[0] != signatureHeader || !strings.HasPrefix(lines[1], "key: ") || !strings.HasPrefix(lines[2], "signature: ") {
		return "", errors.New("malformed release signature")
	}
	id := strings.TrimPrefix(lines[1], "key: ")
	sig, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(lines[2], "signature: "))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", errors.New("malformed release signature")
	}
	for _, k := range keys {
		if k.ID == id {
			if ed25519.Verify(k.Key, signedMessage(tag, sums), sig) {
				return id, nil
			}
			return "", fmt.Errorf("release %s signature does not verify with key %s", tag, id)
		}
	}
	return "", fmt.Errorf("release %s is signed by key %s, which this build does not trust", tag, id)
}
