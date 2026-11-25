package bpsauth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Credential is a flat BPS OAuth file, also readable by bps-local --auth.
// The provider stays separate from Codex, whose OAuth client is different.
type Credential struct {
	DeviceCredentials
	Type      string `json:"type"`
	AccountID string `json:"account_id"`
	Subject   string `json:"sub,omitempty"`
	Email     string `json:"email,omitempty"`
	Expired   string `json:"expired"`
}

func normalizeCredential(credentials DeviceCredentials, accountID, declaredExpiry string) (Credential, error) {
	if credentials.ClientID != ExcelClientID || strings.TrimSpace(credentials.AccessToken) == "" {
		return Credential{}, errors.New("an independent BPS OAuth grant with the BPS client_id is required")
	}
	if credentials.TokenType != "" && !strings.EqualFold(credentials.TokenType, "Bearer") {
		return Credential{}, errors.New("BPS token_type must be Bearer")
	}
	credentials.TokenType = "Bearer"
	claims := decodeJWT(credentials.AccessToken)
	if clientID, _ := claims["client_id"].(string); clientID != "" && clientID != ExcelClientID {
		return Credential{}, errors.New("OAuth token belongs to a different client")
	}
	fromToken := AccountIDFromToken(credentials.AccessToken)
	if fromToken == "" {
		fromToken = AccountIDFromToken(credentials.IDToken)
	}
	if fromToken != "" && accountID != "" && fromToken != accountID {
		return Credential{}, errors.New("BPS account identity mismatch")
	}
	if fromToken != "" {
		accountID = fromToken
	}
	if strings.TrimSpace(accountID) == "" {
		return Credential{}, errors.New("BPS OAuth credentials are missing an account identity")
	}
	expires := jwtExpiry(credentials.AccessToken)
	if declaredExpiry != "" {
		declared, errParse := time.Parse(time.RFC3339, declaredExpiry)
		if errParse != nil {
			return Credential{}, errors.New("invalid BPS credential expiry")
		}
		if expires.IsZero() || declared.Before(expires) {
			expires = declared
		}
	}
	if credentials.ExpiresIn != 0 {
		if credentials.ExpiresIn < 0 || credentials.ExpiresIn > 31536000 {
			return Credential{}, errors.New("invalid BPS expires_in")
		}
		if credentials.ObtainedAt != "" {
			obtained, errParse := time.Parse(time.RFC3339, credentials.ObtainedAt)
			if errParse != nil {
				return Credential{}, errors.New("invalid BPS obtained_at")
			}
			declared := obtained.Add(time.Duration(credentials.ExpiresIn) * time.Second)
			if expires.IsZero() || declared.Before(expires) {
				expires = declared
			}
		}
	}
	if expires.IsZero() {
		return Credential{}, errors.New("BPS OAuth credentials are missing an absolute expiry")
	}
	subject, _ := claims["sub"].(string)
	identity := decodeJWT(credentials.IDToken)
	email, _ := identity["email"].(string)
	return Credential{DeviceCredentials: credentials, Type: "bps", AccountID: accountID,
		Subject: subject, Email: email, Expired: expires.UTC().Format(time.RFC3339)}, nil
}

func parseCredential(raw []byte, explicit bool) (Credential, map[string]any, bool, error) {
	var metadata map[string]any
	if len(raw) > 2<<20 || json.Unmarshal(raw, &metadata) != nil || metadata == nil {
		return Credential{}, nil, explicit, errors.New("invalid BPS credential JSON")
	}
	provider, _ := metadata["type"].(string)
	clientID, _ := metadata["client_id"].(string)
	if provider != "" && provider != "bps" {
		return Credential{}, nil, false, nil
	}
	if !explicit && provider != "bps" && clientID != ExcelClientID {
		return Credential{}, nil, false, nil
	}
	var stored struct {
		Credential
		Tokens *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
		ExpiresAt json.RawMessage `json:"expires_at"`
	}
	if json.Unmarshal(raw, &stored) != nil {
		return Credential{}, nil, true, errors.New("invalid BPS credential fields")
	}
	if stored.Tokens != nil {
		if stored.AccessToken != "" {
			return Credential{}, nil, true, errors.New("ambiguous BPS credential file")
		}
		stored.AccessToken, stored.RefreshToken, stored.IDToken = stored.Tokens.AccessToken, stored.Tokens.RefreshToken, stored.Tokens.IDToken
		stored.AccountID = stored.Tokens.AccountID
	}
	if stored.Expired == "" && len(stored.ExpiresAt) > 0 && string(stored.ExpiresAt) != "null" {
		var seconds int64
		if json.Unmarshal(stored.ExpiresAt, &seconds) == nil && seconds > 0 {
			stored.Expired = time.Unix(seconds, 0).UTC().Format(time.RFC3339)
		} else if json.Unmarshal(stored.ExpiresAt, &stored.Expired) != nil {
			return Credential{}, nil, true, errors.New("invalid BPS expires_at")
		}
	}
	credential, err := normalizeCredential(stored.DeviceCredentials, stored.AccountID, stored.Expired)
	return credential, metadata, true, err
}

func authData(credential Credential, metadata map[string]any) (pluginapi.AuthData, error) {
	encoded, errMarshal := json.Marshal(credential)
	if errMarshal != nil {
		return pluginapi.AuthData{}, errors.New("cannot encode BPS credentials")
	}
	merged := make(map[string]any)
	for key, value := range metadata {
		merged[key] = value
	}
	// Credentials and their lifecycle fields always replace the old token set.
	for _, key := range []string{"tokens", "access_token", "refresh_token", "id_token", "scope", "obtained_at", "expires_in", "expires_at", "expired", "earliest_refresh_at", "email", "sub", "refresh_interval", "refreshInterval", "refresh_interval_seconds", "refreshIntervalSeconds"} {
		delete(merged, key)
	}
	var fields map[string]any
	_ = json.Unmarshal(encoded, &fields)
	for key, value := range fields {
		merged[key] = value
	}
	encoded, errMarshal = json.Marshal(merged)
	if errMarshal != nil {
		return pluginapi.AuthData{}, errors.New("cannot encode BPS metadata")
	}
	identity := sha256.Sum256([]byte(credential.Subject + "\x00" + credential.AccountID + "\x00" + ExcelClientID))
	fileName := "bps-" + hex.EncodeToString(identity[:16]) + ".json"
	prefix, _ := merged["prefix"].(string)
	proxyURL, _ := merged["proxy_url"].(string)
	disabled, _ := merged["disabled"].(bool)
	label := credential.Email
	if label == "" {
		label = "BPS " + hex.EncodeToString(identity[:4])
	}
	return pluginapi.AuthData{Provider: "bps", ID: fileName, FileName: fileName, Label: label,
		Prefix: prefix, ProxyURL: proxyURL, Disabled: disabled, StorageJSON: encoded, Metadata: merged}, nil
}
