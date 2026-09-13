package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/logger"
)

const (
	defaultTimeout     = 30 * time.Second
	userAgent          = "WeKnora-DingTalk-Connector/1.0"
	maxResponseBytes   = 8 << 20 // 8 MiB guard against oversized bodies
	maxListPageSize    = 50      // /v2.0/wiki/nodes caps maxResults at 50
	maxWorkspacePage   = 30      // /v2.0/wiki/workspaces caps maxResults at 30
	tokenRefreshSafety = 2 * time.Minute
)

// client wraps the DingTalk open-platform API.
//
// Two credential families are needed: the new-style accessToken for
// api.dingtalk.com (/v1.0, /v2.0) and the legacy access_token for
// oapi.dingtalk.com (contact endpoints). They are obtained and cached
// separately; whether the new token also works against the legacy host is
// not documented, so the connector does not rely on it (spec R2).
type client struct {
	cfg      *Config
	apiBase  string
	oapiBase string
	http     *http.Client

	mu              sync.Mutex
	apiToken        string
	apiTokenExpiry  time.Time
	oapiAccessToken string
	oapiTokenExpiry time.Time
	// operatorID caches the unionId resolved from OperatorMobile. It is a
	// one-time lookup: the mobile is static configuration, not per-request.
	operatorID string
}

func newClient(cfg *Config) *client {
	return &client{
		cfg:      cfg,
		apiBase:  cfg.GetAPIBaseURL(),
		oapiBase: cfg.GetOAPIBaseURL(),
		http:     datasource.NewConnectorHTTPClient(defaultTimeout),
	}
}

// --- token management ---

// accessToken returns a cached open-platform accessToken, refreshing it when
// it is missing or about to expire.
func (c *client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.apiToken != "" && time.Now().Before(c.apiTokenExpiry) {
		token := c.apiToken
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	body, err := json.Marshal(map[string]string{
		"appKey":    c.cfg.AppKey,
		"appSecret": c.cfg.AppSecret,
	})
	if err != nil {
		return "", fmt.Errorf("marshal token request: %w", err)
	}
	var resp accessTokenResponse
	if err := c.do(ctx, http.MethodPost, c.apiBase+"/v1.0/oauth2/accessToken",
		map[string]string{"Content-Type": "application/json"}, body, &resp); err != nil {
		return "", fmt.Errorf("obtain accessToken: %w", err)
	}
	if resp.AccessToken == "" {
		return "", fmt.Errorf("obtain accessToken: empty token in response")
	}
	ttl := time.Duration(resp.ExpireIn) * time.Second
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}

	c.mu.Lock()
	c.apiToken = resp.AccessToken
	c.apiTokenExpiry = time.Now().Add(ttl - tokenRefreshSafety)
	c.mu.Unlock()
	return resp.AccessToken, nil
}

// oapiToken returns a cached legacy access_token for oapi.dingtalk.com.
func (c *client) oapiToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.oapiAccessToken != "" && time.Now().Before(c.oapiTokenExpiry) {
		token := c.oapiAccessToken
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	q := url.Values{}
	q.Set("appkey", c.cfg.AppKey)
	q.Set("appsecret", c.cfg.AppSecret)
	var resp oapiTokenResponse
	if err := c.do(ctx, http.MethodGet, c.oapiBase+"/gettoken?"+q.Encode(), nil, nil, &resp); err != nil {
		return "", fmt.Errorf("obtain oapi access_token: %w", err)
	}
	if resp.ErrCode != 0 {
		return "", fmt.Errorf("obtain oapi access_token: errcode=%d errmsg=%s", resp.ErrCode, resp.ErrMsg)
	}
	if resp.AccessToken == "" {
		return "", fmt.Errorf("obtain oapi access_token: empty token in response")
	}
	ttl := time.Duration(resp.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}

	c.mu.Lock()
	c.oapiAccessToken = resp.AccessToken
	c.oapiTokenExpiry = time.Now().Add(ttl - tokenRefreshSafety)
	c.mu.Unlock()
	return resp.AccessToken, nil
}

// resolveOperatorID resolves the configured mobile number to a unionId, which
// every Wiki/storage endpoint requires as `operatorId`. The result is cached
// for the lifetime of the client.
func (c *client) resolveOperatorID(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.operatorID != "" {
		id := c.operatorID
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	token, err := c.oapiToken(ctx)
	if err != nil {
		return "", err
	}

	userID, err := c.getUserIDByMobile(ctx, token, c.cfg.OperatorMobile)
	if err != nil {
		return "", err
	}
	unionID, err := c.getUnionIDByUserID(ctx, token, userID)
	if err != nil {
		return "", err
	}
	if unionID == "" {
		return "", fmt.Errorf("resolve operator: user %q has no unionId", userID)
	}

	c.mu.Lock()
	c.operatorID = unionID
	c.mu.Unlock()
	return unionID, nil
}

// getUserIDByMobile maps a phone number to an in-service employee's userId.
// DingTalk only resolves active employees; a departed employee returns an error.
func (c *client) getUserIDByMobile(ctx context.Context, token, mobile string) (string, error) {
	form := url.Values{}
	form.Set("access_token", token)
	form.Set("mobile", mobile)
	var resp oapiGetUserByMobileResponse
	if err := c.do(ctx, http.MethodPost, c.oapiBase+"/topapi/v2/user/getbymobile",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, []byte(form.Encode()), &resp); err != nil {
		return "", fmt.Errorf("query user by mobile: %w", err)
	}
	if resp.ErrCode != 0 {
		return "", fmt.Errorf("query user by mobile: errcode=%d errmsg=%s (is the number an active employee?)",
			resp.ErrCode, resp.ErrMsg)
	}
	if resp.Result.UserID == "" {
		return "", fmt.Errorf("query user by mobile: empty userId for %s", mobile)
	}
	return resp.Result.UserID, nil
}

// getUnionIDByUserID maps a userId to its org-scoped unionId.
func (c *client) getUnionIDByUserID(ctx context.Context, token, userID string) (string, error) {
	form := url.Values{}
	form.Set("access_token", token)
	form.Set("userid", userID)
	var resp oapiGetUserDetailResponse
	if err := c.do(ctx, http.MethodPost, c.oapiBase+"/topapi/v2/user/get",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, []byte(form.Encode()), &resp); err != nil {
		return "", fmt.Errorf("query user detail: %w", err)
	}
	if resp.ErrCode != 0 {
		return "", fmt.Errorf("query user detail: errcode=%d errmsg=%s", resp.ErrCode, resp.ErrMsg)
	}
	return resp.Result.UnionID, nil
}

// --- knowledge base API ---

// listWorkspaces returns one page of knowledge bases visible to operatorID.
func (c *client) listWorkspaces(ctx context.Context, operatorID, nextToken string) ([]workspace, string, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, "", err
	}
	q := url.Values{}
	q.Set("operatorId", operatorID)
	q.Set("maxResults", fmt.Sprintf("%d", maxWorkspacePage))
	if nextToken != "" {
		q.Set("nextToken", nextToken)
	}
	var resp workspaceListResponse
	if err := c.do(ctx, http.MethodGet, c.apiBase+"/v2.0/wiki/workspaces?"+q.Encode(),
		c.authHeader(token), nil, &resp); err != nil {
		return nil, "", err
	}
	return resp.Workspaces, resp.NextToken, nil
}

// listAllWorkspaces pages through every knowledge base visible to operatorID.
func (c *client) listAllWorkspaces(ctx context.Context, operatorID string) ([]workspace, error) {
	var all []workspace
	next := ""
	for {
		page, token, err := c.listWorkspaces(ctx, operatorID, next)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if token == "" || len(page) == 0 {
			return all, nil
		}
		next = token
	}
}

// listNodes returns one page of direct children of parentNodeID.
func (c *client) listNodes(ctx context.Context, operatorID, parentNodeID, nextToken string) ([]wikiNode, string, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, "", err
	}
	q := url.Values{}
	q.Set("parentNodeId", parentNodeID)
	q.Set("operatorId", operatorID)
	q.Set("maxResults", fmt.Sprintf("%d", maxListPageSize))
	if nextToken != "" {
		q.Set("nextToken", nextToken)
	}
	var resp nodeListResponse
	if err := c.do(ctx, http.MethodGet, c.apiBase+"/v2.0/wiki/nodes?"+q.Encode(),
		c.authHeader(token), nil, &resp); err != nil {
		return nil, "", err
	}
	return resp.Nodes, resp.NextToken, nil
}

// listAllNodes pages through every direct child of parentNodeID.
func (c *client) listAllNodes(ctx context.Context, operatorID, parentNodeID string) ([]wikiNode, error) {
	var all []wikiNode
	next := ""
	for {
		page, token, err := c.listNodes(ctx, operatorID, parentNodeID, next)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if token == "" || len(page) == 0 {
			return all, nil
		}
		next = token
	}
}

// queryBlocks returns the first-level block elements of an online document.
// DingTalk returns only top-level blocks; nested blocks (table rows/cells,
// callout children) are handled defensively downstream.
func (c *client) queryBlocks(ctx context.Context, operatorID, docKey string) ([]blockElement, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if operatorID != "" {
		q.Set("operatorId", operatorID)
	}
	base := c.apiBase + "/v1.0/doc/suites/documents/" + url.PathEscape(docKey) + "/blocks"
	if enc := q.Encode(); enc != "" {
		base += "?" + enc
	}
	var resp blocksResponse
	if err := c.do(ctx, http.MethodGet, base, c.authHeader(token), nil, &resp); err != nil {
		return nil, err
	}
	return extractBlocks(resp.Result)
}

// apiStatusError carries the HTTP status of a non-2xx DingTalk response so
// callers can distinguish "document gone" (404) from "no permission" (403)
// without string matching. The message keeps the historical format.
type apiStatusError struct {
	Status int
	Body   string
}

func (e *apiStatusError) Error() string {
	return fmt.Sprintf("dingtalk API error status=%d body=%s", e.Status, e.Body)
}

// authHeader builds the header map for open-platform calls.
func (c *client) authHeader(token string) map[string]string {
	return map[string]string{
		"x-acs-dingtalk-access-token": token,
		"Content-Type":                "application/json",
	}
}

// --- HTTP plumbing ---

// do executes an HTTP request with retries for transient failures (transport
// errors, 429, 5xx) and decodes a JSON response into out. body is re-created
// per attempt so retries are safe, and the request body is never logged.
func (c *client) do(
	ctx context.Context,
	method, rawURL string,
	headers map[string]string,
	body []byte,
	out interface{},
) error {
	backoff := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
	maxRetries := len(backoff)

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		req.Header.Set("User-Agent", userAgent)

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("execute request: %w", err)
			if attempt < maxRetries {
				if sErr := sleepCtx(ctx, backoff[attempt]); sErr != nil {
					return sErr
				}
				continue
			}
			return lastErr
		}

		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("read response: %w", readErr)
			if attempt < maxRetries {
				if sErr := sleepCtx(ctx, backoff[attempt]); sErr != nil {
					return sErr
				}
				continue
			}
			return lastErr
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("dingtalk API transient status=%d body=%s", resp.StatusCode, truncate(string(data), 300))
			if attempt < maxRetries {
				logger.Warnf(ctx, "[DingTalk] %s %s attempt %d/%d: %v", method, redactURL(rawURL), attempt+1, maxRetries+1, lastErr)
				if sErr := sleepCtx(ctx, backoff[attempt]); sErr != nil {
					return sErr
				}
				continue
			}
			return lastErr
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return &apiStatusError{Status: resp.StatusCode, Body: truncate(string(data), 500)}
		}

		if out == nil {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w body=%s", err, truncate(string(data), 300))
		}
		return nil
	}
	return lastErr
}

// extractBlocks decodes the `result` payload of the blocks endpoint into a
// block slice. The exact envelope is not documented, so several plausible
// shapes are accepted:
//
//	[ {...}, ... ]                       — result is the array
//	{ "blocks": [...] }                  — result is an object wrapping an array
//	{ "data": [...] } / { "data": {"blocks": [...]} } — nested variants
func extractBlocks(raw json.RawMessage) ([]blockElement, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var direct []blockElement
	if err := json.Unmarshal(raw, &direct); err == nil {
		return direct, nil
	}
	var wrapper struct {
		Blocks []blockElement  `json:"blocks"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil {
		if len(wrapper.Blocks) > 0 {
			return wrapper.Blocks, nil
		}
		if len(wrapper.Data) > 0 {
			return extractBlocks(wrapper.Data)
		}
	}
	return nil, fmt.Errorf("decode blocks: unrecognized result shape: %s", truncate(string(raw), 200))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// redactURL removes query values (which may carry access_token) from a URL for
// logging. The path alone is enough to identify the call.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparsable url)"
	}
	if u.RawQuery != "" {
		keys := make([]string, 0, 4)
		for k := range u.Query() {
			keys = append(keys, k)
		}
		u.RawQuery = "?" + strings.Join(keys, "&") + "=<redacted>"
	}
	return u.String()
}
