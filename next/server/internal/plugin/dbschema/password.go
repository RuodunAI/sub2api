package dbschema

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// password derives the password of plugin pluginKey's database identity
// from the master key. It is never stored: the core derives it again to
// set it (as a SCRAM verifier) and to log in as the identity.
func (m *Manager) password(pluginKey string) string {
	mac := hmac.New(sha256.New, m.masterKey)
	mac.Write([]byte("sub2api plugin database password\x00" + m.dbOID + "\x00" + pluginKey))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// scramIterations is the iteration count of the verifiers scramVerifier
// writes, PostgreSQL's default (scram_iterations).
const scramIterations = 4096

// scramVerifier is the SCRAM-SHA-256 verifier of password (RFC 5802/7677)
// in PostgreSQL's format, with a random salt: ALTER ROLE ... PASSWORD
// stores it as it is, so the password itself never reaches the server or
// its logs. The password is ASCII (base64url), which SASLprep leaves as it
// is.
func scramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, scramIterations, sha256.Size)
	if err != nil {
		return "", err
	}
	clientKey := hmacSum(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSum(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", scramIterations, b64(salt), b64(storedKey[:]), b64(serverKey)), nil
}

func hmacSum(key []byte, msg string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return mac.Sum(nil)
}
