package model

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const DefaultPlatformOrigin = "https://platform.deepseek.com"

// AccountGrant belongs in the credential store, never in session history.
// DeviceID is stored with the grant so future logins can reuse one device UUID.
type AccountGrant struct {
	Version  int    `json:"version"`
	Token    string `json:"token"`
	Issuer   string `json:"issuer"`
	DeviceID string `json:"device_id,omitempty"`
}

type AccountOptions struct {
	PlatformOrigin        string
	AllowLoopbackHTTP     bool
	HTTPClient            *http.Client
	ClientVersion         string
	Locale                string
	TimezoneOffsetSeconds int
	DeviceID              string
	OSVersion             string
	// CallbackOrigin supports an explicitly forwarded loopback port over SSH.
	// Empty uses a new listener at http://127.0.0.1:<allocated-port>.
	CallbackOrigin string
	RequestTimeout time.Duration
	AttemptTimeout time.Duration
}

type AccountClient struct {
	origin  string
	options AccountOptions
	http    *http.Client
	headers http.Header
}

// NewAccount configures only the official Platform PKCE protocol. HTTP is
// allowed solely on explicitly enabled loopback test/development origins.
func NewAccount(options AccountOptions) (*AccountClient, error) {
	origin := options.PlatformOrigin
	if origin == "" {
		origin = DefaultPlatformOrigin
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || !(u.Scheme == "https" || (options.AllowLoopbackHTTP && u.Scheme == "http" && loopback(u.Hostname()))) {
		return nil, &Error{Code: "ACCOUNT_INVALID_ORIGIN"}
	}
	if options.CallbackOrigin != "" {
		if _, err := callbackOrigin(options.CallbackOrigin); err != nil {
			return nil, err
		}
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = 30 * time.Second
	}
	if options.RequestTimeout > 2*time.Minute {
		options.RequestTimeout = 2 * time.Minute
	}
	if options.AttemptTimeout <= 0 {
		options.AttemptTimeout = 10 * time.Minute
	}
	if options.AttemptTimeout > time.Hour {
		options.AttemptTimeout = time.Hour
	}
	if options.ClientVersion == "" {
		options.ClientVersion = "0.1.0"
	}
	if strings.ContainsAny(options.ClientVersion, "\r\n\x00") {
		return nil, invalid()
	}
	locale := "en_US"
	if strings.HasPrefix(strings.ToLower(options.Locale), "zh") {
		locale = "zh_CN"
	}
	options.Locale = locale
	if options.TimezoneOffsetSeconds == 0 {
		_, options.TimezoneOffsetSeconds = time.Now().Zone()
	}
	if options.OSVersion == "" {
		options.OSVersion = runtime.GOOS + " " + runtime.GOARCH
	}
	if options.DeviceID != "" && !validUUID(options.DeviceID) {
		return nil, &Error{Code: "ACCOUNT_INVALID_DEVICE"}
	}
	client := &http.Client{}
	if options.HTTPClient != nil {
		*client = *options.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	headers := http.Header{"X-Client-Bundle-Id": []string{""}, "X-Client-Platform": []string{"web"}, "X-Client-Version": []string{options.ClientVersion}, "X-Client-Locale": []string{locale}, "X-Client-Timezone-Offset": []string{strconv.Itoa(options.TimezoneOffsetSeconds)}}
	return &AccountClient{origin: u.Scheme + "://" + u.Host, options: options, http: client, headers: headers}, nil
}

// Login starts a one-use loopback callback, publishes a verified Platform URL
// through authorize, exchanges the PKCE code, then calls save before redirecting
// the browser. save must atomically store the grant outside session history.
// The module never opens a browser itself and never prints credentials or URLs.
func (a *AccountClient) Login(ctx context.Context, authorize func(string) error, save func(AccountGrant) error) (AccountGrant, error) {
	if authorize == nil || save == nil {
		return AccountGrant{}, &Error{Code: "ACCOUNT_INVALID_CALLBACK"}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return AccountGrant{}, &Error{Code: "ACCOUNT_CALLBACK_UNAVAILABLE"}
	}
	attempt, cancel := context.WithTimeout(ctx, a.options.AttemptTimeout)
	defer cancel()
	verifier, err := randomToken()
	if err != nil {
		listener.Close()
		return AccountGrant{}, err
	}
	state, err := randomToken()
	if err != nil {
		listener.Close()
		return AccountGrant{}, err
	}
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	callback := "http://" + listener.Addr().String()
	if a.options.CallbackOrigin != "" {
		callback = a.options.CallbackOrigin
	}
	callback, err = callbackOrigin(callback)
	if err != nil {
		listener.Close()
		return AccountGrant{}, err
	}
	redirectURI := callback + "/oauth/callback"
	type reply struct {
		url    string
		status int
	}
	type callbackRequest struct {
		code  string
		reply chan reply
		done  chan struct{}
	}
	requests := make(chan callbackRequest, 1)
	var mu sync.Mutex
	ready, used := false, false
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		query := r.URL.Query()
		receivedState := query.Get("state")
		code := query.Get("code")
		if r.Method != "GET" || r.URL.Path != "/oauth/callback" || len(query["state"]) != 1 || len(query["code"]) != 1 || len(code) == 0 || len(code) > 8192 || len(receivedState) != len(state) || subtle.ConstantTimeCompare([]byte(receivedState), []byte(state)) != 1 {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		available := ready && !used && attempt.Err() == nil
		if available {
			used = true
		}
		mu.Unlock()
		if !available {
			w.WriteHeader(410)
			return
		}
		request := callbackRequest{code: code, reply: make(chan reply, 1), done: make(chan struct{})}
		defer close(request.done)
		select {
		case requests <- request:
		case <-attempt.Done():
			w.WriteHeader(410)
			return
		}
		select {
		case result := <-request.reply:
			if result.url != "" {
				w.Header().Set("Location", result.url)
			}
			w.WriteHeader(result.status)
		case <-attempt.Done():
			w.WriteHeader(410)
		case <-r.Context().Done():
		}
	})
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	var init struct {
		AuthorizeURL string  `json:"authorize_url"`
		AuthorizeID  string  `json:"authorize_id"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if err = a.platform(attempt, "/auth-api/v0/dsh/auth_init", "", map[string]any{"code_challenge": challenge, "code_challenge_method": "S256", "state": state, "redirect_uri": redirectURI, "locale": a.options.Locale, "login_source": "web"}, &init); err != nil {
		return AccountGrant{}, err
	}
	if init.AuthorizeID == "" || init.ExpiresIn <= 0 {
		return AccountGrant{}, &Error{Code: "ACCOUNT_PROTOCOL"}
	}
	authorizeURL, err := a.browserURL(init.AuthorizeURL, "/dsh/authorize")
	if err != nil {
		return AccountGrant{}, err
	}
	committed := false
	defer func() {
		if !committed && attempt.Err() != nil {
			go func() {
				cancelCtx, cancel := context.WithTimeout(context.Background(), a.options.RequestTimeout)
				defer cancel()
				_ = a.platform(cancelCtx, "/auth-api/v0/dsh/auth_cancel", "", map[string]string{"authorize_id": init.AuthorizeID, "code_verifier": verifier}, nil)
			}()
		}
	}()
	remaining := time.Duration(min(init.ExpiresIn, 3600) * float64(time.Second))
	expiry := time.AfterFunc(remaining, cancel)
	defer expiry.Stop()
	mu.Lock()
	ready = true
	mu.Unlock()
	if err = authorize(authorizeURL); err != nil {
		cancel()
		return AccountGrant{}, &Error{Code: "ACCOUNT_BROWSER_UNAVAILABLE"}
	}
	var request callbackRequest
	select {
	case request = <-requests:
	case <-attempt.Done():
		return AccountGrant{}, accountContextError(ctx, attempt)
	}
	completion := ""
	defer func() {
		status := http.StatusNoContent
		if completion != "" {
			status = http.StatusFound
		}
		request.reply <- reply{url: completion, status: status}
		select {
		case <-request.done:
		case <-time.After(time.Second):
		}
	}()
	deviceID := a.options.DeviceID
	if deviceID == "" {
		deviceID, err = randomUUID()
		if err != nil {
			return AccountGrant{}, err
		}
	}
	var exchange struct {
		Token         string `json:"token"`
		AuthorizedURL string `json:"authorized_url"`
	}
	if err = a.platform(attempt, "/auth-api/v0/dsh/auth_exchange", "", map[string]string{"code": request.code, "code_verifier": verifier, "redirect_uri": redirectURI, "device_id": deviceID, "device_model": nodeOS() + "-" + nodeArch(), "os_version": a.options.OSVersion}, &exchange); err != nil {
		return AccountGrant{}, err
	}
	if !validToken(exchange.Token) {
		return AccountGrant{}, &Error{Code: "ACCOUNT_PROTOCOL"}
	}
	completion, err = a.browserURL(exchange.AuthorizedURL, "/dsh/authorized")
	if err != nil {
		return AccountGrant{}, err
	}
	finish, _ := url.Parse(completion)
	q := finish.Query()
	q.Set("login_source", "web")
	finish.RawQuery = q.Encode()
	completion = finish.String()
	if attempt.Err() != nil {
		completion = ""
		return AccountGrant{}, accountContextError(ctx, attempt)
	}
	grant := AccountGrant{Version: 1, Token: exchange.Token, Issuer: a.origin, DeviceID: deviceID}
	// Once persistence is admitted, cancellation cannot claim that no login was
	// saved. The callback completes only after the atomic save has settled.
	if err = save(grant); err != nil {
		completion = ""
		return AccountGrant{}, &Error{Code: "ACCOUNT_STORAGE"}
	}
	committed = true
	return grant, nil
}

// Logout revokes a captured old grant. The caller removes local credentials
// before this call, so a network failure never restores a signed-out account.
func (a *AccountClient) Logout(ctx context.Context, grant AccountGrant) error {
	if err := a.validateGrant(grant); err != nil {
		return err
	}
	return a.platform(ctx, "/auth-api/v0/users/logout", grant.Token, nil, nil)
}

// Profile returns only display fields from Platform's masked account profile.
func (a *AccountClient) Profile(ctx context.Context, grant AccountGrant) (AccountProfile, error) {
	if err := a.validateGrant(grant); err != nil {
		return AccountProfile{}, err
	}
	var raw struct {
		ID        string `json:"id"`
		IDProfile *struct {
			Name    *string `json:"name"`
			Picture *string `json:"picture"`
		} `json:"id_profile"`
		Mobile       string  `json:"mobile"`
		MobileNumber string  `json:"mobile_number"`
		Email        *string `json:"email"`
	}
	if err := a.platform(ctx, "/auth-api/v0/users/current", grant.Token, nil, &raw); err != nil {
		return AccountProfile{}, err
	}
	if raw.Email == nil {
		return AccountProfile{}, &Error{Code: "ACCOUNT_PROTOCOL"}
	}
	contact := raw.Mobile
	if contact == "" {
		contact = raw.MobileNumber
	}
	if contact == "" {
		contact = *raw.Email
	}
	result := AccountProfile{ID: raw.ID, Contact: contact}
	if raw.IDProfile != nil {
		result.AvatarURL = raw.IDProfile.Picture
		if raw.IDProfile.Name != nil {
			result.Name = *raw.IDProfile.Name
		}
	}
	return result, nil
}

type AccountProfile struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	AvatarURL *string `json:"avatar_url,omitempty"`
	Contact   string  `json:"contact,omitempty"`
}

func (a *AccountClient) validateGrant(grant AccountGrant) error {
	if grant.Version != 1 || grant.Issuer != a.origin || !validToken(grant.Token) {
		return &Error{Code: "ACCOUNT_INVALID_GRANT"}
	}
	return nil
}
func (a *AccountClient) browserURL(value, path string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme+"://"+u.Host != a.origin || u.Path != path || u.User != nil || u.Fragment != "" {
		return "", &Error{Code: "ACCOUNT_PROTOCOL"}
	}
	return u.String(), nil
}
func callbackOrigin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || !loopback(u.Hostname()) || u.Port() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", &Error{Code: "ACCOUNT_CALLBACK_ORIGIN"}
	}
	port, e := strconv.Atoi(u.Port())
	if e != nil || port < 1 || port > 65535 {
		return "", &Error{Code: "ACCOUNT_CALLBACK_ORIGIN"}
	}
	return "http://" + u.Host, nil
}
func loopback(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }
func randomToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", &Error{Code: "ACCOUNT_RANDOM"}
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
func randomUUID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", &Error{Code: "ACCOUNT_RANDOM"}
	}
	data[6] = data[6]&0x0f | 0x40
	data[8] = data[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", data[0:4], data[4:6], data[6:8], data[8:10], data[10:]), nil
}
func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, b := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if b != '-' {
				return false
			}
		} else if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F') {
			return false
		}
	}
	return true
}
func validToken(token string) bool {
	if token == "" {
		return false
	}
	for _, b := range []byte(token) {
		if b < 0x21 || b > 0x7e {
			return false
		}
	}
	return true
}
func nodeOS() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}
func nodeArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	}
	return runtime.GOARCH
}
func accountContextError(parent, attempt context.Context) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if attempt.Err() != nil {
		return &Error{Code: "ACCOUNT_EXPIRED"}
	}
	return nil
}

func (a *AccountClient) platform(ctx context.Context, path, token string, body any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, a.options.RequestTimeout)
	defer cancel()
	var reader io.Reader
	method := http.MethodPost
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return invalid()
		}
		reader = bytes.NewReader(data)
	} else if path == "/auth-api/v0/users/current" {
		method = http.MethodGet
	}
	request, err := http.NewRequestWithContext(ctx, method, a.origin+path, reader)
	if err != nil {
		return &Error{Code: "ACCOUNT_PROTOCOL"}
	}
	request.Header = a.headers.Clone()
	request.Header.Set("User-Agent", "dsgo/0.1 (DeepSeek account terminal client)")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("x-dsh-auth-token", token)
	}
	response, err := a.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{Code: "ACCOUNT_NO_RESPONSE"}
	}
	defer response.Body.Close()
	if response.StatusCode == 401 && token != "" {
		return &Error{Code: "ACCOUNT_EXPIRED", Status: 401}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &Error{Code: "ACCOUNT_HTTP", Status: response.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil {
		return &Error{Code: "ACCOUNT_PROTOCOL"}
	}
	var envelope struct {
		Code *int `json:"code"`
		Data *struct {
			BizCode *int            `json:"biz_code"`
			BizData json.RawMessage `json:"biz_data"`
		} `json:"data"`
	}
	if len(data) > 65536 || json.Unmarshal(data, &envelope) != nil {
		return &Error{Code: "ACCOUNT_PROTOCOL"}
	}
	if envelope.Code != nil && *envelope.Code == 40003 && token != "" {
		return &Error{Code: "ACCOUNT_EXPIRED"}
	}
	if envelope.Code == nil || *envelope.Code != 0 || envelope.Data == nil || envelope.Data.BizCode == nil || *envelope.Data.BizCode != 0 {
		return &Error{Code: "ACCOUNT_PROTOCOL"}
	}
	if out != nil && json.Unmarshal(envelope.Data.BizData, out) != nil {
		return &Error{Code: "ACCOUNT_PROTOCOL"}
	}
	return nil
}
