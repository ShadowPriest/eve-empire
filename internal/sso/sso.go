// Package sso implements the EVE Online SSO (OAuth 2.0) flow:
// authorize redirect, code exchange, token refresh and JWT validation.
package sso

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	authorizeURL = "https://login.eveonline.com/v2/oauth/authorize"
	tokenURL     = "https://login.eveonline.com/v2/oauth/token"
	jwksURL      = "https://login.eveonline.com/oauth/jwks"
	issuer       = "https://login.eveonline.com"
	audience     = "EVE Online"
)

type Client struct {
	ClientID     string
	ClientSecret string
	CallbackURL  string
	Scopes       []string
	UserAgent    string

	http *http.Client

	jwksMu      sync.Mutex
	jwksKeys    map[string]*rsa.PublicKey
	jwksFetched time.Time
}

func New(clientID, clientSecret, callbackURL string, scopes []string, userAgent string) *Client {
	return &Client{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		CallbackURL:  callbackURL,
		Scopes:       scopes,
		UserAgent:    userAgent,
		http:         &http.Client{Timeout: 15 * time.Second},
	}
}

// AuthorizeURL builds the login.eveonline.com redirect URL.
func (c *Client) AuthorizeURL(state string) string {
	q := url.Values{
		"response_type": {"code"},
		"redirect_uri":  {c.CallbackURL},
		"client_id":     {c.ClientID},
		"scope":         {strings.Join(c.Scopes, " ")},
		"state":         {state},
	}
	return authorizeURL + "?" + q.Encode()
}

// Token is the response from the SSO token endpoint.
type Token struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
}

// Exchange swaps an authorization code for tokens.
func (c *Client) Exchange(code string) (*Token, error) {
	return c.tokenRequest(url.Values{
		"grant_type": {"authorization_code"},
		"code":       {code},
	})
}

// Refresh obtains a new access token using a refresh token.
func (c *Client) Refresh(refreshToken string) (*Token, error) {
	return c.tokenRequest(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

func (c *Client) tokenRequest(form url.Values) (*Token, error) {
	req, err := http.NewRequest("POST", tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.ClientID, c.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return nil, fmt.Errorf("sso token endpoint: %s (%s: %s)", resp.Status, e.Error, e.Description)
	}

	var t Token
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, err
	}
	return &t, nil
}

// CharacterClaims is the useful subset of the EVE SSO JWT.
type CharacterClaims struct {
	CharacterID   int64
	CharacterName string
	Scopes        []string
	ExpiresAt     time.Time
}

// VerifyToken validates the JWT signature against the SSO JWKS and
// extracts character identity from the claims.
func (c *Client) VerifyToken(accessToken string) (*CharacterClaims, error) {
	tok, err := jwt.Parse(accessToken, c.keyFunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
	)
	if err != nil {
		return nil, fmt.Errorf("jwt validation: %w", err)
	}

	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("unexpected claims type")
	}

	sub, _ := claims["sub"].(string)
	// sub format: "CHARACTER:EVE:2112345678"
	parts := strings.Split(sub, ":")
	if len(parts) != 3 || parts[0] != "CHARACTER" {
		return nil, fmt.Errorf("unexpected sub claim: %q", sub)
	}
	charID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("bad character id in sub %q: %w", sub, err)
	}

	name, _ := claims["name"].(string)

	var scopes []string
	switch scp := claims["scp"].(type) {
	case string:
		scopes = []string{scp}
	case []any:
		for _, s := range scp {
			if str, ok := s.(string); ok {
				scopes = append(scopes, str)
			}
		}
	}

	exp := time.Now()
	if e, err := claims.GetExpirationTime(); err == nil && e != nil {
		exp = e.Time
	}

	return &CharacterClaims{
		CharacterID:   charID,
		CharacterName: name,
		Scopes:        scopes,
		ExpiresAt:     exp,
	}, nil
}

// keyFunc resolves the RSA public key for a JWT by kid, fetching and
// caching the JWKS document from the SSO.
func (c *Client) keyFunc(t *jwt.Token) (any, error) {
	kid, _ := t.Header["kid"].(string)

	c.jwksMu.Lock()
	defer c.jwksMu.Unlock()

	if key, ok := c.jwksKeys[kid]; ok {
		return key, nil
	}
	// Refresh the JWKS at most once a minute (covers key rotation).
	if time.Since(c.jwksFetched) < time.Minute {
		return nil, fmt.Errorf("unknown jwt key id %q", kid)
	}
	if err := c.fetchJWKSLocked(); err != nil {
		return nil, err
	}
	if key, ok := c.jwksKeys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("unknown jwt key id %q", kid)
}

func (c *Client) fetchJWKSLocked() error {
	req, err := http.NewRequest("GET", jwksURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()

	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey)
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(nb),
			E: int(new(big.Int).SetBytes(eb).Int64()),
		}
	}
	if len(keys) == 0 {
		return fmt.Errorf("jwks contained no RSA keys")
	}
	c.jwksKeys = keys
	c.jwksFetched = time.Now()
	return nil
}
