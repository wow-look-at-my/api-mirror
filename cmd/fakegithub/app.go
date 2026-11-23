// app.go holds fakeApp.

package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	userInstallationPath = regexp.MustCompile(`^/users/([^/]+)/installation$`)
	accessTokensPath     = regexp.MustCompile(`^/app/installations/([0-9]+)/access_tokens$`)
)

// fakeApp plays a GitHub App's installation endpoints.
type fakeApp struct {
	id  string
	key *rsa.PrivateKey

	mu        sync.Mutex
	exchanges int
}

func newFakeApp(id, keyPath string) (*fakeApp, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, block, 0o600); err != nil {
		return nil, err
	}
	return &fakeApp{id: id, key: key}, nil
}

// verify reports whether the request carries a JWT this app's key signed.
func (a *fakeApp) verify(r *http.Request) error {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return fmt.Errorf("no bearer")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fmt.Errorf("not a JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&a.key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		return err
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var claims struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return err
	}
	if claims.Iss != a.id || time.Now().Unix() >= claims.Exp {
		return fmt.Errorf("wrong issuer or expired")
	}
	return nil
}

// answer serves an App endpoint, and reports false for any other path.
func (a *fakeApp) answer(w http.ResponseWriter, r *http.Request) bool {
	owner := userInstallationPath.FindStringSubmatch(r.URL.Path)
	tokens := accessTokensPath.FindStringSubmatch(r.URL.Path)
	if owner == nil && (tokens == nil || r.Method != http.MethodPost) {
		return false
	}
	if err := a.verify(r); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "A JSON web token could not be decoded: " + err.Error()})
		return true
	}
	if owner != nil {
		_ = json.NewEncoder(w).Encode(doc{"id": installationID(owner[1]), "account": user(owner[1]), "app_id": a.id})
		return true
	}
	a.mu.Lock()
	a.exchanges++
	a.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(doc{
		"token":       "ghs_fake_installation_" + tokens[1],
		"expires_at":  time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"permissions": doc{"contents": "read", "metadata": "read"},
	})
	return true
}

// installationID is a stable id, so each owner has its own token.
func installationID(owner string) int {
	id := 1000
	for _, c := range strings.ToLower(owner) {
		id = id*31 + int(c)
		id %= 1_000_000
	}
	return id
}

func (a *fakeApp) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.exchanges
}
