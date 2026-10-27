package webtools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

func validateURL(raw string) (*url.URL, error) {
	if len([]rune(raw)) > 2048 {
		return nil, webError("WEB_FETCH_URL_INVALID", "URL exceeds 2048 characters")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" {
		return nil, webError("WEB_FETCH_URL_INVALID", "fetch requires an HTTP(S) URL without embedded credentials")
	}
	if strings.Contains(u.Hostname(), "%") {
		return nil, webError("WEB_FETCH_URL_INVALID", "zone-qualified destinations are not allowed")
	}
	u.Fragment = ""
	u.RawFragment = ""
	return u, nil
}

func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		return strings.ToLower(u.Scheme + "://" + u.Hostname())
	}
	return strings.ToLower(u.Scheme + "://" + net.JoinHostPort(u.Hostname(), port))
}

var deniedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/3"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range deniedPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func dialPublic(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") || strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".localhost") {
		return nil, webError("WEB_FETCH_BLOCKED", "anonymous fetch requires a public destination")
	}
	answers, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(answers) == 0 {
		return nil, errors.New("fetch DNS returned no addresses")
	}
	for _, answer := range answers {
		ip, ok := netip.AddrFromSlice(answer.IP)
		if !ok || answer.Zone != "" || !publicIP(ip) {
			return nil, webError("WEB_FETCH_BLOCKED", "anonymous fetch rejects non-public DNS answers")
		}
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	var last error
	for _, answer := range answers {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(answer.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

func fetchPublic(ctx context.Context, raw string) (FetchResult, error) {
	transport := &http.Transport{Proxy: nil, DialContext: dialPublic, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	return fetchWithClient(ctx, raw, &http.Client{Transport: transport})
}

func fetchWithClient(ctx context.Context, raw string, baseClient *http.Client) (FetchResult, error) {
	u, err := validateURL(raw)
	if err != nil {
		return FetchResult{}, err
	}
	initialOrigin := origin(u)
	client := *baseClient
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			return webError("WEB_FETCH_REDIRECT_LIMIT", "fetch exceeds five redirects")
		}
		if _, err := validateURL(req.URL.String()); err != nil {
			return err
		}
		if origin(req.URL) != initialOrigin {
			return webError("WEB_FETCH_BLOCKED", "anonymous fetch rejects a redirect to another origin")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return FetchResult{}, err
	}
	req.Header.Set("User-Agent", "dsgo/0.1.0 (DeepSeek Harness terminal port)")
	req.Header.Set("Accept", "text/html, text/plain, application/json, application/xml;q=0.9, */*;q=0.5")
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return FetchResult{}, webError("WEB_ABORTED", "web fetch cancelled")
		}
		var we *WebError
		if errors.As(err, &we) {
			return FetchResult{}, we
		}
		return FetchResult{}, webError("WEB_FETCH_FAILED", "anonymous HTTP fetch failed")
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 5000001))
	if err != nil {
		if ctx.Err() != nil {
			return FetchResult{}, webError("WEB_ABORTED", "web fetch cancelled")
		}
		return FetchResult{}, webError("WEB_FETCH_FAILED", "could not read response body")
	}
	truncated := len(body) > 5000000
	if truncated {
		body = body[:5000000]
	}
	mediaType, params, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || mediaType == "" {
		mediaType, params, _ = mime.ParseMediaType(http.DetectContentType(body))
	}
	kind := "text"
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" {
		kind = "html"
	} else if !strings.HasPrefix(mediaType, "text/") && mediaType != "application/json" && mediaType != "application/xml" && !strings.HasSuffix(mediaType, "+json") && !strings.HasSuffix(mediaType, "+xml") {
		return FetchResult{}, webError("WEB_FETCH_UNSUPPORTED_CONTENT", fmt.Sprintf("response content type %q is not text", mediaType))
	}
	if bytes.IndexByte(body, 0) >= 0 {
		return FetchResult{}, webError("WEB_FETCH_UNSUPPORTED_CONTENT", "response contains binary NUL bytes")
	}
	text, err := decodeBody(body, strings.ToLower(params["charset"]))
	if err != nil {
		return FetchResult{}, err
	}
	if len([]rune(text)) > 100000 {
		text = bounded(text, 100000)
		truncated = true
	}
	return FetchResult{URL: res.Request.URL.String(), StatusCode: res.StatusCode, Kind: kind, Body: text, Truncated: truncated}, nil
}

func decodeBody(body []byte, charset string) (string, error) {
	switch charset {
	case "", "utf-8", "utf8", "us-ascii":
		return strings.ToValidUTF8(string(body), "�"), nil
	case "iso-8859-1", "latin1", "latin-1":
		r := make([]rune, len(body))
		for i, b := range body {
			r[i] = rune(b)
		}
		return string(r), nil
	case "windows-1252":
		mapping := []rune{'€', '�', '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', '�', 'Ž', '�', '�', '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', '�', 'ž', 'Ÿ'}
		r := make([]rune, len(body))
		for i, b := range body {
			if b >= 0x80 && b <= 0x9f {
				r[i] = mapping[b-0x80]
			} else {
				r[i] = rune(b)
			}
		}
		return string(r), nil
	default:
		return "", webError("WEB_FETCH_UNSUPPORTED_CONTENT", "unsupported response text charset "+charset)
	}
}
