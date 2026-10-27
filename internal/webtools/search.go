package webtools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"dsgo/internal/core"
)

func endpoint(base, operation string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", webError("WEB_PROVIDER_UNAVAILABLE", "invalid independent web API base URL")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + operation
	return u.String(), nil
}

func (e *Executor) dispatch(ctx context.Context, target string, body any, messages bool, record func() error) (json.RawMessage, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	token := ""
	if e.options.ResolveToken != nil {
		token, err = e.options.ResolveToken(ctx, target)
		if err != nil {
			return nil, webError("WEB_PROVIDER_ERROR", "DeepSeek account token resolution failed")
		}
	} else if origin(u) == e.options.TokenOrigin {
		token = e.options.Token
	}
	if token == "" && e.options.APIKey == "" {
		return nil, webError("WEB_PROVIDER_CREDENTIAL_MISSING", "DeepSeek web access needs DEEPSEEK_API_KEY or a DeepSeek account sign-in authorized for this endpoint")
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "dsgo/0.1.0 (DeepSeek Harness terminal port)")
	if token != "" {
		req.Header.Set("x-dsh-auth-token", token)
	} else {
		req.Header.Set("Authorization", "Bearer "+e.options.APIKey)
		if messages {
			req.Header.Set("x-api-key", e.options.APIKey)
		}
	}
	if messages {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	client := http.Client{}
	if e.options.HTTPClient != nil {
		client = *e.options.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if ctx.Err() != nil {
		return nil, webError("WEB_ABORTED", "DeepSeek web operation cancelled")
	}
	if record != nil {
		if err := record(); err != nil {
			return nil, fmt.Errorf("persist auxiliary search request: %w", err)
		}
	}
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, webError("WEB_ABORTED", "DeepSeek web operation cancelled")
		}
		return nil, webError("WEB_PROVIDER_ERROR", "DeepSeek web request failed; check the independent web endpoint "+target)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 5000001))
	if err != nil {
		if ctx.Err() != nil {
			return nil, webError("WEB_ABORTED", "DeepSeek web operation cancelled")
		}
		return nil, webError("WEB_PROVIDER_ERROR", "failed reading DeepSeek web response")
	}
	if len(data) > 5000000 {
		return nil, webError("WEB_PROVIDER_ERROR", "DeepSeek web response exceeds 5000000 bytes")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == 401 && token != "" {
			return nil, webError("WEB_PROVIDER_ERROR", "DeepSeek account web request was rejected; sign in again")
		}
		return nil, webError("WEB_PROVIDER_ERROR", fmt.Sprintf("DeepSeek web API returned HTTP %d; check the independent endpoint %s", response.StatusCode, target))
	}
	if !json.Valid(data) {
		return nil, webError("WEB_PROVIDER_ERROR", "DeepSeek web API returned invalid JSON")
	}
	return data, nil
}

func (e *Executor) messagesSearch(ctx context.Context, tc core.ToolContext, query string) ([]Source, error) {
	target, err := endpoint(e.options.SearchBaseURL, "messages")
	if err != nil {
		return nil, err
	}
	body := map[string]any{"model": e.options.SearchModel, "max_tokens": e.options.SearchMaxTokens, "messages": []core.Message{core.TextMessage("user", "Perform a web search for the query: "+query)}, "tools": []map[string]any{{"type": "web_search_20250305", "name": "web_search", "max_uses": e.options.SearchMaxUses}}, "thinking": map[string]string{"type": "disabled"}}
	data, err := e.dispatch(ctx, target, body, true, func() error {
		if tc.Emit != nil {
			return tc.Emit("web/deepseek-search-llm-request", map[string]any{"endpoint": target, "apiVersion": "2023-06-01", "body": body})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var response struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, webError("WEB_PROVIDER_ERROR", "invalid DeepSeek Messages search response")
	}
	type searchBlock struct {
		Type      string          `json:"type"`
		Content   json.RawMessage `json:"content"`
		Citations []struct {
			URL       string `json:"url"`
			CitedText string `json:"cited_text"`
		} `json:"citations"`
	}
	blocks := make([]searchBlock, len(response.Content))
	snippets := map[string]string{}
	for i, raw := range response.Content {
		if err := json.Unmarshal(raw, &blocks[i]); err != nil {
			return nil, webError("WEB_PROVIDER_ERROR", "invalid search content block")
		}
		if blocks[i].Type == "text" {
			for _, cite := range blocks[i].Citations {
				if cite.URL != "" && cite.CitedText != "" && snippets[cite.URL] == "" {
					snippets[cite.URL] = cite.CitedText
				}
			}
		}
	}
	sources := []Source{}
	seen := map[string]bool{}
	found := false
	for _, block := range blocks {
		if block.Type != "web_search_tool_result" {
			continue
		}
		found = true
		var items []struct {
			Type    string `json:"type"`
			URL     string `json:"url"`
			Title   string `json:"title"`
			PageAge string `json:"page_age"`
		}
		if err := json.Unmarshal(block.Content, &items); err != nil {
			return nil, webError("WEB_PROVIDER_ERROR", "DeepSeek native search returned a tool error")
		}
		for _, item := range items {
			if item.Type != "web_search_result" || item.URL == "" || seen[item.URL] {
				continue
			}
			seen[item.URL] = true
			sources = append(sources, Source{URL: item.URL, Title: item.Title, PublishedAt: item.PageAge, Snippet: snippets[item.URL]})
		}
	}
	if !found {
		return nil, webError("WEB_PROVIDER_ERROR", "DeepSeek returned no web_search_tool_result blocks; no prose fallback is used")
	}
	if len(sources) > e.options.SearchMaxResults {
		sources = sources[:e.options.SearchMaxResults]
	}
	return sources, nil
}

func (e *Executor) directSearch(ctx context.Context, tc core.ToolContext, query string) ([]Source, error) {
	if e.options.DirectAPICount < 1 || e.options.DirectAPICount > 50 {
		return nil, webError("WEB_PROVIDER_UNAVAILABLE", "DeepSeek Search API count must be 1 through 50")
	}
	target, err := endpoint(e.options.DirectAPIBaseURL, "search")
	if err != nil {
		return nil, err
	}
	count := e.options.DirectAPICount
	if e.options.SearchMaxResults < count {
		count = e.options.SearchMaxResults
	}
	body := map[string]any{"query": query, "count": count, "need_content": false}
	if !e.options.OmitOriginalQuestion && e.options.OriginalQuestion != nil {
		question := strings.Join(strings.Fields(e.options.OriginalQuestion(tc.SessionID)), " ")
		if question != "" {
			body["ori_question"] = bounded(question, 128)
		}
	}
	data, err := e.dispatch(ctx, target, body, false, nil)
	if err != nil {
		return nil, err
	}
	var response struct {
		Results json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(data, &response); err != nil || len(response.Results) == 0 || string(response.Results) == "null" {
		return nil, webError("WEB_PROVIDER_ERROR", "invalid DeepSeek Search API results")
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(response.Results, &items); err != nil {
		return nil, webError("WEB_PROVIDER_ERROR", "invalid DeepSeek Search API results")
	}
	sources := make([]Source, 0, len(items))
	for _, item := range items {
		var source Source
		var date *string
		if json.Unmarshal(item["url"], &source.URL) != nil || source.URL == "" || json.Unmarshal(item["title"], &source.Title) != nil || json.Unmarshal(item["snippet"], &source.Snippet) != nil || len(item["date"]) == 0 || json.Unmarshal(item["date"], &date) != nil {
			return nil, webError("WEB_PROVIDER_ERROR", "invalid DeepSeek Search API result entry")
		}
		if date != nil {
			source.PublishedAt = *date
		}
		sources = append(sources, source)
	}
	return sources, nil
}

func (e *Executor) directFetch(ctx context.Context, requested string) (FetchResult, error) {
	if _, err := validateURL(requested); err != nil {
		return FetchResult{}, err
	}
	target, err := endpoint(e.options.DirectAPIBaseURL, "fetch")
	if err != nil {
		return FetchResult{}, err
	}
	data, err := e.dispatch(ctx, target, map[string]string{"url": requested}, false, nil)
	if err != nil {
		return FetchResult{}, err
	}
	var response struct {
		Content *string `json:"content"`
	}
	if err := json.Unmarshal(data, &response); err != nil || response.Content == nil {
		return FetchResult{}, webError("WEB_PROVIDER_ERROR", "invalid DeepSeek Fetch API content")
	}
	body := *response.Content
	truncated := len([]rune(body)) > 200000
	return FetchResult{URL: requested, StatusCode: 200, Kind: "text", Body: bounded(body, 200000), Truncated: truncated}, nil
}
