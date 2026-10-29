package mcp

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// fetch follows the redirect behavior used by the SDK's default global fetch.
// http.Client's built-in policy differs for methods and cross-origin headers,
// so each explicit MCP hop is sent with its automatic redirect handling disabled.
// See Undici v7.16.0 lib/web/fetch/index.js:httpRedirectFetch and constants.js.
func (h *httpTransport) fetch(req *http.Request) (*http.Response, error) {
	for redirects := 0; ; redirects++ {
		res, err := h.client.Do(req)
		if err != nil {
			return nil, err
		}
		switch res.StatusCode {
		case 301, 302, 303, 307, 308:
		default:
			return res, nil
		}
		locations, present := res.Header["Location"]
		if !present {
			return res, nil
		}
		location, err := req.URL.Parse(strings.Join(locations, ", "))
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		if (location.Scheme != "http" && location.Scheme != "https") || location.User != nil {
			return nil, errors.New("MCP redirect requires an HTTP(S) URL without credentials")
		}
		if redirects == 20 {
			return nil, errors.New("MCP redirect count exceeded")
		}
		next := req.Clone(req.Context())
		next.URL = location
		next.Response = res
		next.RequestURI = ""
		next.Body = nil
		if req.Body != nil && req.Body != http.NoBody {
			if req.GetBody == nil {
				return nil, errors.New("MCP redirect body cannot be replayed")
			}
			next.Body, err = req.GetBody()
			if err != nil {
				return nil, err
			}
		}
		if ((res.StatusCode == 301 || res.StatusCode == 302) && req.Method == http.MethodPost) ||
			(res.StatusCode == 303 && req.Method != http.MethodGet && req.Method != http.MethodHead) {
			if next.Body != nil {
				next.Body.Close()
			}
			next.Method, next.Body, next.GetBody, next.ContentLength = http.MethodGet, nil, nil, 0
			for _, name := range []string{"Content-Encoding", "Content-Language", "Content-Location", "Content-Type", "Content-Length"} {
				next.Header.Del(name)
			}
		}
		if !sameHTTPOrigin(req.URL, location) {
			// Custom MCP and user headers continue. Fetch's sensitive-header
			// list is deliberately narrower than "all custom headers".
			for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Host"} {
				next.Header.Del(name)
			}
			next.Host = ""
		}
		req = next
	}
}

func sameHTTPOrigin(a, b *url.URL) bool {
	origin := func(u *url.URL) (string, string, string) {
		scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
		if ip := net.ParseIP(host); ip != nil {
			host = ip.String()
		} else if ascii, err := idna.Lookup.ToASCII(host); err == nil {
			host = ascii
		}
		if port == "" {
			if scheme == "https" {
				port = "443"
			} else if scheme == "http" {
				port = "80"
			}
		}
		return scheme, host, port
	}
	as, ah, ap := origin(a)
	bs, bh, bp := origin(b)
	return as == bs && ah == bh && ap == bp
}
