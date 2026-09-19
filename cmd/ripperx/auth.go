package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Authentication is a single fixed account, kept in the settings file, and a
// signed token proving a browser has already presented it. That is the whole
// model: ripperX drives one scanner for one household, so there is nothing to
// gain from user records, sessions held in memory, or a second factor.
//
// The tokens are standard HS256 JWTs, so the same credentials work for the page
// and for a script using Authorization: Bearer.
//
// There are two of them. The access token is short-lived, so a copy that leaks
// out of a browser - a proxy log, a shared machine - is worthless within the
// quarter hour. The refresh token is long-lived and does nothing but mint
// access tokens, which is what keeps a household from logging in every morning.
// Both are cookies; a request carrying a stale access token and a good refresh
// token is renewed in passing, so nothing in the page has to know this exists.
const (
	sessionCookie = "ripperx_token"
	refreshCookie = "ripperx_refresh"
	// loginDelay is charged to every failed login. It costs an honest user
	// nothing and takes an online guessing attack from thousands of tries a
	// second to three.
	loginDelay = 300 * time.Millisecond

	accessKind  = "access"
	refreshKind = "refresh"
)

type auth struct {
	user       string
	pass       string
	secret     []byte
	ttl        time.Duration
	refreshTTL time.Duration
}

// newAuth builds the authenticator, or returns nil when no credentials are
// configured - ripperX then serves as it always has, unauthenticated. Half a
// credential is a configuration mistake, not a request to run open.
//
// An unset jwt-secret gets a random one, which is the right default: tokens
// then stop working when ripperX restarts, and nothing on disk has to be
// protected. Setting it keeps logins alive across restarts.
func newAuth(user, pass, secretHex string, ttl, refreshTTL time.Duration) (*auth, error) {
	if user == "" && pass == "" {
		return nil, nil
	}
	if user == "" || pass == "" {
		return nil, errors.New("auth-user and auth-password must both be set, or neither")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("auth-ttl must be positive, got %s", ttl)
	}
	if refreshTTL < ttl {
		return nil, fmt.Errorf("auth-refresh-ttl (%s) must be at least auth-ttl (%s)", refreshTTL, ttl)
	}
	var secret []byte
	if secretHex == "" {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("generating a signing key: %w", err)
		}
	} else {
		b, err := hex.DecodeString(secretHex)
		if err != nil {
			return nil, fmt.Errorf("jwt-secret: expected hex, %w", err)
		}
		if len(b) < 32 {
			return nil, fmt.Errorf("jwt-secret: %d bytes of key material, want at least 32", len(b))
		}
		secret = b
	}
	return &auth{user: user, pass: pass, secret: secret, ttl: ttl, refreshTTL: refreshTTL}, nil
}

type claims struct {
	Sub string `json:"sub"`
	// Kind separates the two tokens, so a refresh token cannot be presented as
	// an access token and thereby outlive its 15 minutes.
	Kind string `json:"kind"`
	Iat  int64  `json:"iat"`
	Exp  int64  `json:"exp"`
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (a *auth) sign(signingInput string) []byte {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(signingInput))
	return m.Sum(nil)
}

// mint returns a token of the given kind, good for ttl from now.
func (a *auth) mint(kind string, ttl time.Duration, now time.Time) (string, error) {
	head := b64([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, err := json.Marshal(claims{
		Sub:  a.user,
		Kind: kind,
		Iat:  now.Unix(),
		Exp:  now.Add(ttl).Unix(),
	})
	if err != nil {
		return "", err
	}
	input := head + "." + b64(body)
	return input + "." + b64(a.sign(input)), nil
}

// verify checks the signature first and the contents second, so a forged token
// is rejected before anything inside it is believed. The subject is checked
// against the configured user: changing auth-user invalidates tokens that named
// the old one.
func (a *auth) verify(token, kind string, now time.Time) error {
	head, rest, ok := strings.Cut(token, ".")
	if !ok {
		return errors.New("malformed token")
	}
	body, sig, ok := strings.Cut(rest, ".")
	if !ok {
		return errors.New("malformed token")
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return errors.New("malformed token")
	}
	if !hmac.Equal(got, a.sign(head+"."+body)) {
		return errors.New("bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return errors.New("malformed token")
	}
	var c claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return errors.New("malformed token")
	}
	if c.Sub != a.user {
		return errors.New("token is for another user")
	}
	if c.Kind != kind {
		return fmt.Errorf("token is a %s token, want %s", c.Kind, kind)
	}
	if c.Exp <= now.Unix() {
		return errors.New("token has expired")
	}
	return nil
}

// setTokens issues a fresh pair and puts both in cookies. The access token is
// what every request is judged on; the refresh token is only ever read when the
// access token has expired.
func (a *auth) setTokens(w http.ResponseWriter, now time.Time) (access, refresh string, err error) {
	access, err = a.mint(accessKind, a.ttl, now)
	if err != nil {
		return "", "", err
	}
	refresh, err = a.mint(refreshKind, a.refreshTTL, now)
	if err != nil {
		return "", "", err
	}
	a.setCookie(w, sessionCookie, access, a.ttl)
	a.setCookie(w, refreshCookie, refresh, a.refreshTTL)
	return access, refresh, nil
}

func (a *auth) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:  name,
		Value: value,
		Path:  "/",
		// The tokens are never read by page script, and SameSite=Strict keeps
		// another site from spending them on a scan in the background - which
		// is what stands in for a CSRF token here.
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Secure is deliberately not set: ripperX is normally reached over plain
		// HTTP on a LAN, and a Secure cookie would simply never be stored.
		MaxAge: int(ttl / time.Second),
	})
}

// tokenFrom takes the named cookie's token, or - for the access token only -
// an Authorization header, for callers that are not a browser. A non-browser
// client refreshes explicitly at /api/refresh, so the header never carries a
// refresh token.
func tokenFrom(r *http.Request, cookie string) string {
	if cookie == sessionCookie {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			return strings.TrimPrefix(h, "Bearer ")
		}
		// A media player is not a browser: VLC opens a playlist URL with no
		// cookie jar and no way to be told a header. So the access token may
		// also ride in the query string, but only on the endpoints that
		// serve media - never on one that starts a job or writes a disc.
		if allowsQueryToken(r.URL.Path) {
			if t := r.URL.Query().Get("access_token"); t != "" {
				return t
			}
		}
	}
	if c, err := r.Cookie(cookie); err == nil {
		return c.Value
	}
	return ""
}

// allowsQueryToken lists the endpoints a player may reach with a token in
// the URL. They are all reads, and all of them are what goes into a
// playlist. A token in a URL is a token in a proxy log, which is why the
// list is this short and why access tokens are short-lived.
func allowsQueryToken(path string) bool {
	if strings.HasPrefix(path, "/api/images/") {
		return true
	}
	if !strings.HasPrefix(path, "/api/drives/") {
		return false
	}
	rest := path[len("/api/drives/"):]
	_, tail, _ := strings.Cut(rest, "/")
	return tail == "file" || tail == "tar" || tail == "playlist.m3u" ||
		strings.HasPrefix(tail, "audio/")
}

type loginRequest struct {
	User     string `json:"user" doc:"the configured auth-user"`
	Password string `json:"password" doc:"the configured auth-password"`
}

// tokenResponse is what a login or a refresh hands back. A browser uses the
// cookies set alongside it and ignores this; a script sends the token as
// Authorization: Bearer and comes back to /api/refresh with the other one.
type tokenResponse struct {
	Token            string `json:"token" doc:"short-lived access token, sent on every request"`
	RefreshToken     string `json:"refreshToken" doc:"long-lived token, only accepted by /api/refresh"`
	ExpiresIn        int    `json:"expiresIn" doc:"seconds the access token is valid for"`
	RefreshExpiresIn int    `json:"refreshExpiresIn" doc:"seconds the refresh token is valid for"`
}

// handleLogin exchanges the configured credentials for a token. Both fields are
// compared in constant time, and a wrong user and a wrong password are reported
// identically, so a failure says nothing about which half was wrong.
func (a *auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "malformed request"})
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(req.User), []byte(a.user))
	passOK := subtle.ConstantTimeCompare([]byte(req.Password), []byte(a.pass))
	if userOK&passOK != 1 {
		time.Sleep(loginDelay)
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "wrong user name or password"})
		return
	}
	access, refresh, err := a.setTokens(w, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{
		Token:            access,
		RefreshToken:     refresh,
		ExpiresIn:        int(a.ttl / time.Second),
		RefreshExpiresIn: int(a.refreshTTL / time.Second),
	})
}

// handleRefresh trades a refresh token for a new pair. A browser never needs to
// call it - guard renews in passing - but a script holding a bearer token does,
// and it can present the refresh token in the Authorization header here because
// this one endpoint expects that kind.
func (a *auth) handleRefresh(w http.ResponseWriter, r *http.Request) {
	token := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token = strings.TrimPrefix(h, "Bearer ")
	} else {
		token = tokenFrom(r, refreshCookie)
	}
	if err := a.verify(token, refreshKind, time.Now()); err != nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: err.Error()})
		return
	}
	access, refresh, err := a.setTokens(w, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{
		Token:            access,
		RefreshToken:     refresh,
		ExpiresIn:        int(a.ttl / time.Second),
		RefreshExpiresIn: int(a.refreshTTL / time.Second),
	})
}

func (a *auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{sessionCookie, refreshCookie} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/",
			HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1,
		})
	}
	writeJSON(w, http.StatusOK, statusMessage{Status: "logged out"})
}

// openPaths are reachable without a token: the login page, the endpoint it
// posts to, and the icon the browser fetches for it.
var openPaths = map[string]bool{
	"/login":       true,
	"/api/login":   true,
	"/api/logout":  true,
	"/api/refresh": true,
	"/favicon.svg": true,
}

// guard rejects anything without a valid access token, first giving a valid
// refresh token the chance to mint one - so a browser sitting on the page over
// a long scan is never thrown out at the 15-minute mark. An API call that
// cannot be renewed gets 401 and JSON, so a stale page can notice and send the
// user back to the login form; a page request is redirected there directly.
func (a *auth) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if openPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		now := time.Now()
		err := a.verify(tokenFrom(r, sessionCookie), accessKind, now)
		if err != nil {
			if rerr := a.verify(tokenFrom(r, refreshCookie), refreshKind, now); rerr == nil {
				if _, _, merr := a.setTokens(w, now); merr == nil {
					err = nil
				}
			}
		}
		if err != nil {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, http.StatusUnauthorized, errorResponse{Error: err.Error()})
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}
