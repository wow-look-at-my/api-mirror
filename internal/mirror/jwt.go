package mirror

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"text/template"
	"time"
)

// engineFuncs are the template functions the mirror adds to the spec
// language. They give a spec the credentials its background work needs.
var engineFuncs = template.FuncMap{
	"jwt":      signedJWT,
	"readFile": readSpecFile,
}

// jwtLifetime is how long a minted JWT is valid.
const jwtLifetime = 9 * time.Minute

// jwtSkew backdates the issue time, so a clock slightly ahead of the upstream's does not make the token not yet valid.
const jwtSkew = time.Minute

type cachedJWT struct {
	token   string
	expires time.Time
}

var (
	jwtMu    sync.Mutex
	jwtCache = map[string]cachedJWT{}
)

// signedJWT mints an RS256 JWT with iss=issuer, signed by an RSA private
// key in PEM form. A key passed through an environment variable often
// arrives with its newlines escaped, so a literal \n is read as a newline.
// A token is reused until a minute before it expires.
func signedJWT(issuer, key string) (string, error) {
	issuer = strings.TrimSpace(issuer)
	key = strings.TrimSpace(key)
	if issuer == "" || key == "" {
		return "", errors.New("jwt: the issuer and the key are both required")
	}
	sum := sha256.Sum256([]byte(issuer + "\x00" + key))
	cacheKey := string(sum[:])
	now := time.Now()

	jwtMu.Lock()
	defer jwtMu.Unlock()
	if c, ok := jwtCache[cacheKey]; ok && now.Before(c.expires.Add(-time.Minute)) {
		return c.token, nil
	}
	priv, err := parseRSAKey(key)
	if err != nil {
		return "", err
	}
	expires := now.Add(jwtLifetime)
	header, err := marshalJSON(map[string]any{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := marshalJSON(map[string]any{
		"iss": issuer,
		"iat": now.Add(-jwtSkew).Unix(),
		"exp": expires.Unix(),
	})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("jwt: sign: %w", err)
	}
	token := signing + "." + enc.EncodeToString(sig)
	jwtCache[cacheKey] = cachedJWT{token: token, expires: expires}
	return token, nil
}

func parseRSAKey(key string) (*rsa.PrivateKey, error) {
	key = strings.ReplaceAll(key, `\n`, "\n")
	block, _ := pem.Decode([]byte(key))
	if block == nil {
		return nil, errors.New("jwt: the key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("jwt: the key is neither PKCS#1 nor PKCS#8: %w", err)
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("jwt: the key is not an RSA key")
	}
	return k, nil
}

// readSpecFile reads a file a spec names, such as a key mounted as a file.
// An empty path reads as empty, so a spec can offer a file or a variable.
func readSpecFile(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("readFile: %w", err)
	}
	return string(b), nil
}
