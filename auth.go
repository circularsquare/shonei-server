package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ---------------------------------------------------------------------------
// Accounts & auth — username/password registration, login, and HMAC-signed
// session tokens. Phase 0 of the account system (see plans/account-system.md).
//
// Identity model: a player's identity is a lowercased username. /register and
// /login (plain HTTP, JSON) issue a stateless HMAC token carrying the username
// and an expiry. The WebSocket handshake (serveWs) trades that token for the
// connection's identity instead of the old free-text ?name=.
//
// Insecure dev mode: when SHONEI_SECRET is unset (i.e. local dev, not the
// deployed box), tokens are signed with a fixed dev secret AND serveWs still
// accepts ?name= so the CLI test client keeps working. Production sets
// SHONEI_SECRET via systemd, which both enables real signing and requires a
// token on every WS connect.
// ---------------------------------------------------------------------------

const (
	// TokenTTL is how long a login stays valid. Stateless HMAC tokens can't be
	// revoked before expiry (a known limitation — logout only clears the client
	// cache), so this is the real session lifetime. Generous for a friends' game.
	TokenTTL = 30 * 24 * time.Hour

	// AccountsFile is the on-disk account store, written next to traders.json and
	// deliberately NOT shipped by deploy.ps1 (like pricelog/traderstock) so a
	// redeploy never wipes accounts.
	AccountsFile = "accounts.json"

	bcryptCost = bcrypt.DefaultCost
)

// usernameRe bounds usernames to a delimiter-safe, URL-safe charset. The token
// format splits on '|', and names flow through query strings and (Phase 2) file
// paths — restricting the charset keeps all of those unambiguous.
var usernameRe = regexp.MustCompile(`^[a-z0-9_]{3,20}$`)

// ── Auth config (set once in initAuth) ─────────────────────────────────────

var (
	authSecret   []byte // HMAC signing key
	insecureMode bool   // true when SHONEI_SECRET unset: accept ?name=, warn loudly
)

func initAuth() {
	if s := os.Getenv("SHONEI_SECRET"); s != "" {
		authSecret = []byte(s)
		insecureMode = false
	} else {
		authSecret = []byte("dev-insecure-secret-do-not-use-in-prod")
		insecureMode = true
		log.Printf("[auth] SHONEI_SECRET unset — INSECURE DEV MODE: ?name= accepted, tokens signed with a fixed dev key")
	}
	loadAccounts()
}

// ── Account store ───────────────────────────────────────────────────────────

// Account is one registered player. PassHash is a bcrypt hash; the plaintext
// password is never stored or logged.
type Account struct {
	PassHash string `json:"passHash"`
	Created  int64  `json:"created"` // unix seconds
}

var (
	accountsMu sync.Mutex
	accounts   = map[string]Account{} // key: lowercased username
)

// loadAccounts reads accounts.json at startup. A missing file is fine — it just
// yields an empty store (no accounts registered yet).
func loadAccounts() {
	accountsMu.Lock()
	defer accountsMu.Unlock()
	data, err := os.ReadFile(AccountsFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[auth] read accounts failed: %v", err)
		}
		return
	}
	if err := json.Unmarshal(data, &accounts); err != nil {
		log.Printf("[auth] parse accounts failed, starting empty: %v", err)
		accounts = map[string]Account{}
		return
	}
	log.Printf("[auth] loaded %d accounts", len(accounts))
}

// saveAccounts writes the store to disk atomically (temp file + rename), the
// same pattern pricelog.go uses. Caller must hold accountsMu.
func saveAccounts() {
	data, err := json.Marshal(accounts)
	if err != nil {
		log.Printf("[auth] marshal accounts failed: %v", err)
		return
	}
	tmp := AccountsFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil { // 0600: contains password hashes
		log.Printf("[auth] write accounts failed: %v", err)
		return
	}
	if err := os.Rename(tmp, AccountsFile); err != nil {
		log.Printf("[auth] rename accounts failed: %v", err)
	}
}

// ── Reserved names (NPC traders) ────────────────────────────────────────────

var reservedNames = map[string]bool{}

// initReservedNames blocks registration of any name an NPC trader uses, so a
// player can't connect as "fulan" and share an ownership string with the bot.
// Call after initDynamicTraders has populated dynamicTraders.
func initReservedNames() {
	for _, t := range dynamicTraders {
		reservedNames[strings.ToLower(t.name)] = true
	}
	reservedNames["anonymous"] = true // the old empty-name sentinel
	log.Printf("[auth] reserved %d names", len(reservedNames))
}

// ── Tokens (stateless, HMAC-signed) ─────────────────────────────────────────
//
// Format: base64url(username) "|" expiryUnix "|" base64url(hmacSHA256(payload)).
// The username charset excludes '|', so the split is unambiguous.

func makeToken(username string) string {
	exp := strconv.FormatInt(time.Now().Add(TokenTTL).Unix(), 10)
	u := base64.RawURLEncoding.EncodeToString([]byte(username))
	payload := u + "|" + exp
	return payload + "|" + sign(payload)
}

func sign(payload string) string {
	m := hmac.New(sha256.New, authSecret)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

var (
	errBadToken     = errors.New("malformed token")
	errBadSignature = errors.New("bad signature")
	errExpiredToken = errors.New("token expired")
)

// verifyToken checks the signature and expiry, returning the username on success.
func verifyToken(token string) (string, error) {
	parts := strings.Split(token, "|")
	if len(parts) != 3 {
		return "", errBadToken
	}
	payload := parts[0] + "|" + parts[1]
	// Constant-time compare so token validity doesn't leak via timing.
	if subtle.ConstantTimeCompare([]byte(parts[2]), []byte(sign(payload))) != 1 {
		return "", errBadSignature
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", errBadToken
	}
	if time.Now().Unix() > exp {
		return "", errExpiredToken
	}
	ub, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", errBadToken
	}
	return string(ub), nil
}

// ── HTTP handlers ────────────────────────────────────────────────────────────

type authRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
type authResponse struct {
	Token    string `json:"token,omitempty"`
	Username string `json:"username,omitempty"`
	Error    string `json:"error,omitempty"`
}

func writeAuthError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(authResponse{Error: msg})
}

func writeAuthOK(w http.ResponseWriter, username, token string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(authResponse{Token: token, Username: username})
}

// parseAuthRequest decodes + normalizes a register/login body. Username is
// lowercased (identity is case-insensitive); password is returned as-is.
func parseAuthRequest(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	if r.Method != http.MethodPost {
		writeAuthError(w, http.StatusMethodNotAllowed, "POST only")
		return "", "", false
	}
	var req authRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeAuthError(w, http.StatusBadRequest, "bad request body")
		return "", "", false
	}
	return strings.ToLower(strings.TrimSpace(req.Username)), req.Password, true
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	username, password, ok := parseAuthRequest(w, r)
	if !ok {
		return
	}
	if !usernameRe.MatchString(username) {
		writeAuthError(w, http.StatusBadRequest, "username must be 3-20 chars, a-z 0-9 _")
		return
	}
	if len(password) < 6 || len(password) > 128 {
		writeAuthError(w, http.StatusBadRequest, "password must be 6-128 chars")
		return
	}
	if reservedNames[username] {
		writeAuthError(w, http.StatusConflict, "name reserved")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "hash failed")
		return
	}

	accountsMu.Lock()
	if _, exists := accounts[username]; exists {
		accountsMu.Unlock()
		writeAuthError(w, http.StatusConflict, "username taken")
		return
	}
	accounts[username] = Account{PassHash: string(hash), Created: time.Now().Unix()}
	saveAccounts()
	accountsMu.Unlock()

	log.Printf("[auth] registered %q", username)
	writeAuthOK(w, username, makeToken(username))
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	username, password, ok := parseAuthRequest(w, r)
	if !ok {
		return
	}

	accountsMu.Lock()
	acc, exists := accounts[username]
	accountsMu.Unlock()

	// Compare even when the account is missing would be ideal to avoid user
	// enumeration via timing, but bcrypt on a fixed dummy hash is the usual
	// trick; for a friends' game a plain 401 is fine.
	if !exists || bcrypt.CompareHashAndPassword([]byte(acc.PassHash), []byte(password)) != nil {
		writeAuthError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	log.Printf("[auth] login %q", username)
	writeAuthOK(w, username, makeToken(username))
}

// authenticateWs resolves the connecting player's identity for serveWs. With a
// valid ?token= it returns the token's username. In insecure dev mode it falls
// back to ?name= (so the CLI test client keeps working); otherwise a missing or
// bad token is rejected before the WebSocket upgrade.
func authenticateWs(r *http.Request) (string, bool) {
	if tok := r.URL.Query().Get("token"); tok != "" {
		name, err := verifyToken(tok)
		if err != nil {
			log.Printf("[auth] ws token rejected: %v", err)
			return "", false
		}
		return name, true
	}
	if insecureMode {
		name := r.URL.Query().Get("name")
		if name == "" {
			name = "anonymous"
		}
		return name, true
	}
	log.Printf("[auth] ws connect rejected: missing token")
	return "", false
}
