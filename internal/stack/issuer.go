package stack

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/luizeduardocarvalho/fixlab/internal/profile"
)

// The jwt-issuer module stands in for a hosted identity provider: nginx
// serving a JWKS over HTTPS, with a key pair and certificate generated per
// run. The certificate is meant to be the app's only trust root (through the
// ca_file output), so an accidental call to a real external HTTPS service
// fails instead of leaving the machine.

const issuerKeyID = "eval-key-1"

// User is one entry of a jwt-issuer users file: the claims to sign, and data
// copied next to the token into tokens.json for the load script (IDs the
// user may touch, and so on).
type User struct {
	Claims map[string]any `json:"claims"`
	Data   map[string]any `json:"data"`
}

func issuerHost(name string) string { return name + ".eval.local" }

func issuerService(name, assets string) map[string]any {
	dir := filepath.Join(assets, name)
	return map[string]any{
		"image": nginxImage,
		"volumes": []string{
			filepath.Join(dir, "nginx.conf") + ":/etc/nginx/conf.d/default.conf:ro",
			assets + ":" + assetsMount + ":ro",
		},
		"networks":    map[string]any{"default": map[string]any{"aliases": []string{issuerHost(name)}}},
		"healthcheck": healthcheck("wget -qO- http://127.0.0.1/healthz", 30),
	}
}

// prepareIssuer writes into dir:
//
//	tls.crt, tls.key  served by nginx; tls.crt is the ca_file output
//	jwks.json         the public signing key, at /.well-known/jwks.json
//	nginx.conf        the server config
//	tokens.json       one RS256 token per user in the users file, with its data
func prepareIssuer(p *profile.Profile, name string, s profile.Service, dir string) (map[string]string, error) {
	host := issuerHost(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := writeTLS(dir, host); err != nil {
		return nil, err
	}
	signer, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	jwks := map[string]any{"keys": []map[string]string{{
		"kid": issuerKeyID,
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(signer.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(signer.E)).Bytes()),
	}}}
	if err := writeJSON(filepath.Join(dir, "jwks.json"), jwks); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "nginx.conf"), []byte(nginxConf(name)), 0o644); err != nil {
		return nil, err
	}

	var users []User
	if s.Users != "" {
		raw, err := os.ReadFile(filepath.Join(p.Dir, s.Users))
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&users); err != nil {
			return nil, fmt.Errorf("parse %s: %w", s.Users, err)
		}
	}
	now := time.Now()
	tokens := make([]map[string]any, 0, len(users))
	for i, u := range users {
		claims := maps.Clone(u.Claims)
		if claims == nil {
			claims = map[string]any{}
		}
		claims["iat"] = now.Unix()
		claims["nbf"] = now.Add(-time.Minute).Unix()
		claims["exp"] = now.Add(6 * time.Hour).Unix()
		tok, err := signJWT(signer, claims)
		if err != nil {
			return nil, fmt.Errorf("user %d: %w", i, err)
		}
		entry := maps.Clone(u.Data)
		if entry == nil {
			entry = map[string]any{}
		}
		entry["token"] = tok
		tokens = append(tokens, entry)
	}
	if err := writeJSON(filepath.Join(dir, "tokens.json"), tokens); err != nil {
		return nil, err
	}

	outs := map[string]string{
		"host":        host,
		"jwks_url":    "https://" + host + "/.well-known/jwks.json",
		"ca_file":     assetsMount + "/" + name + "/tls.crt",
		"tokens_file": assetsMount + "/" + name + "/tokens.json",
	}
	if s.Preset == "clerk" {
		// Clerk SDKs derive the Frontend API host, and so the JWKS URL, from
		// the publishable key: pk_test_ + base64("<host>$").
		outs["publishable_key"] = "pk_test_" + base64.RawURLEncoding.EncodeToString([]byte(host+"$"))
	}
	return outs, nil
}

func nginxConf(name string) string {
	return fmt.Sprintf(`server {
  listen 443 ssl;
  server_name %[1]s;
  ssl_certificate     %[2]s/%[3]s/tls.crt;
  ssl_certificate_key %[2]s/%[3]s/tls.key;

  location = /.well-known/jwks.json {
    default_type application/json;
    alias %[2]s/%[3]s/jwks.json;
  }
  location / { return 404; }
}

server {
  listen 80;
  location = /healthz { return 200 "ok"; }
}
`, issuerHost(name), assetsMount, name)
}

func writeTLS(dir, host string) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o644); err != nil {
		return err
	}
	// World-readable so the nginx worker user can load it; the key is
	// throwaway and only ever trusted by this run's app container.
	return os.WriteFile(filepath.Join(dir, "tls.key"), keyPEM, 0o644)
}

func signJWT(key *rsa.PrivateKey, claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": issuerKeyID})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
