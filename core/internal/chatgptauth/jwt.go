package chatgptauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// claims are the ID token claims that are checked or kept.
type claims struct {
	Issuer  string          `json:"iss"`
	Subject string          `json:"sub"`
	Aud     json.RawMessage `json:"aud"`
	Expiry  int64           `json:"exp"`
	Nonce   string          `json:"nonce"`
	Email   string          `json:"email"`
	Name    string          `json:"name"`
}

// clockSkew is the slack allowed on an expiry. An ID token is checked once,
// seconds after it was issued, so this only absorbs a clock that is a little off.
const clockSkew = time.Minute

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// verifyIDToken checks the ID token as the documentation says to: the
// signature against the published keys, the issuer, the audience (the issued
// client id), the expiry and the nonce of this attempt.
//
// The documentation names no signing algorithm. RS256 and ES256 are accepted
// and anything else, including "none", is refused.
func (a *Auth) verifyIDToken(ctx context.Context, token, clientID, nonce string) (claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims{}, errors.New("the ID token is not a signed token")
	}

	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodePart(parts[0], &header); err != nil {
		return claims{}, errors.New("the ID token header cannot be read")
	}
	if header.Alg != "RS256" && header.Alg != "ES256" {
		return claims{}, fmt.Errorf("the ID token is signed with %q, which is not accepted", header.Alg)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return claims{}, errors.New("the ID token signature cannot be read")
	}

	found, err := a.discover(ctx)
	if err != nil || found.JWKSURI == "" {
		return claims{}, errors.New("could not read the published signing keys")
	}
	var published struct {
		Keys []jwk `json:"keys"`
	}
	if err := a.getJSON(ctx, found.JWKSURI, &published); err != nil {
		return claims{}, errors.New("could not read the published signing keys")
	}

	signed := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !verifiedByAny(published.Keys, header.Alg, header.Kid, signed[:], signature) {
		return claims{}, errors.New("the ID token signature does not match the published signing keys")
	}

	var c claims
	if err := decodePart(parts[1], &c); err != nil {
		return claims{}, errors.New("the ID token claims cannot be read")
	}
	switch {
	case c.Issuer != a.ep.Auth:
		return claims{}, errors.New("the ID token was issued by someone else")
	case !audienceHas(c.Aud, clientID):
		return claims{}, errors.New("the ID token is for a different client")
	case a.now().After(time.Unix(c.Expiry, 0).Add(clockSkew)):
		return claims{}, errors.New("the ID token has expired")
	case subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(nonce)) != 1 || nonce == "":
		return claims{}, errors.New("the ID token does not answer this sign-in (nonce)")
	case c.Subject == "":
		return claims{}, errors.New("the ID token names no subject")
	}
	return c, nil
}

func decodePart(part string, into any) error {
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

func audienceHas(raw json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, aud := range many {
			if aud == want {
				return true
			}
		}
	}
	return false
}

// verifiedByAny: a key with the token's kid, or any key of the right type when
// the token names none.
func verifiedByAny(keys []jwk, alg, kid string, digest, signature []byte) bool {
	for _, key := range keys {
		if kid != "" && key.Kid != kid {
			continue
		}
		switch {
		case alg == "RS256" && key.Kty == "RSA":
			n, e := bigOf(key.N), bigOf(key.E)
			if n == nil || e == nil || !e.IsInt64() {
				continue
			}
			pub := &rsa.PublicKey{N: n, E: int(e.Int64())}
			if rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest, signature) == nil {
				return true
			}
		case alg == "ES256" && key.Kty == "EC" && key.Crv == "P-256":
			x, y := bigOf(key.X), bigOf(key.Y)
			if x == nil || y == nil || len(signature) != 64 {
				continue
			}
			pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
			r := new(big.Int).SetBytes(signature[:32])
			s := new(big.Int).SetBytes(signature[32:])
			if ecdsa.Verify(pub, digest, r, s) {
				return true
			}
		}
	}
	return false
}

func bigOf(encoded string) *big.Int {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) == 0 {
		return nil
	}
	return new(big.Int).SetBytes(raw)
}
