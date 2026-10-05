package server

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"time"
)

var b64 = base64.RawURLEncoding

func (p *Pool) jwk() map[string]any {
	pub := p.cfg.Key.PublicKey
	return map[string]any{
		"kid": p.cfg.KID, "kty": "RSA", "alg": "RS256", "use": "sig",
		"n": b64.EncodeToString(pub.N.Bytes()),
		"e": b64.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func (p *Pool) jwt(claims map[string]any) (string, error) {
	header, _ := json.Marshal(map[string]string{"kid": p.cfg.KID, "alg": "RS256"})
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := b64.EncodeToString(header) + "." + b64.EncodeToString(body)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(nil, p.cfg.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return input + "." + b64.EncodeToString(sig), nil
}

func (p *Pool) verify(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&p.cfg.Key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return nil
	}
	raw, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	if exp, _ := claims["exp"].(float64); exp < float64(time.Now().Unix()) {
		return nil
	}
	if claims["iss"] != p.Issuer() {
		return nil
	}
	return claims
}

type tokenOpts struct {
	ttl      *int
	issuer   string
	clientID string
}

func (p *Pool) claims(u *user, use string, o tokenOpts) map[string]any {
	now := time.Now().Unix()
	ttl := int(p.cfg.TokenTTL / time.Second)
	if o.ttl != nil {
		ttl = *o.ttl
	}
	issuer := o.issuer
	if issuer == "" {
		issuer = p.Issuer()
	}
	cid := o.clientID
	if cid == "" {
		cid = p.cfg.ClientID
	}
	c := map[string]any{
		"sub": u.Sub, "iss": issuer, "token_use": use,
		"auth_time": now, "iat": now, "exp": now + int64(ttl),
		"jti": uuid4(), "origin_jti": uuid4(), "event_id": uuid4(),
	}
	if use == "access" {
		c["client_id"] = cid
		c["scope"] = "aws.cognito.signin.user.admin"
		c["username"] = u.Username
		return c
	}
	c["aud"] = cid
	c["cognito:username"] = u.Username
	c["email"] = u.Attrs["email"]
	c["email_verified"] = u.Attrs["email_verified"] == "true"
	c["given_name"] = u.Attrs["given_name"]
	c["family_name"] = u.Attrs["family_name"]
	return c
}

func (p *Pool) token(u *user, use string, o tokenOpts) (string, error) {
	return p.jwt(p.claims(u, use, o))
}
