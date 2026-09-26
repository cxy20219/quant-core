// Package tushare 是 Tushare Pro HTTP API 的 Go 客户端。
//
// 协议与 tushare pro 官方一致:
//
//	POST http://api.tushare.pro
//	{"api_name": "stock_basic", "token": "...", "params": {...}, "fields": "..."}
//
// 响应:
//
//	{"code": 0, "msg": null, "data": {"fields": [...], "items": [[...]], "has_more": false}}
package tushare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DefaultURL 是 tushare 官方接口地址。
const DefaultURL = "https://api.tushare.pro"

// Client 是 tushare pro 客户端。
type Client struct {
	URL    string
	Token  string
	HTTP   *http.Client
	Limit  int // 单次请求行数(分页步长),默认 5000
	Logf   func(format string, args ...any)
	Pause  time.Duration // 每次请求后的休眠,避免触发频控
}

// NewClient 创建客户端。
func NewClient(token string) *Client {
	return &Client{
		URL:   DefaultURL,
		Token: token,
		HTTP:  &http.Client{Timeout: 60 * time.Second},
		Limit: 5000,
	}
}

type request struct {
	APIName string         `json:"api_name"`
	Token   string         `json:"token"`
	Params  map[string]any `json:"params"`
	Fields  string         `json:"fields"`
}

type response struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data *struct {
		Fields  []string `json:"fields"`
		Items   [][]any  `json:"items"`
		HasMore bool     `json:"has_more"`
	} `json:"data"`
}

// Result 是单次调用的结果。
type Result struct {
	Fields  []string
	Items   [][]any
	HasMore bool
}

// Call 执行一次 API 调用(不含分页)。
func (c *Client) Call(ctx context.Context, apiName string, params map[string]any, fields string) (*Result, error) {
	if params == nil {
		params = map[string]any{}
	}
	payload, err := json.Marshal(request{APIName: apiName, Token: c.Token, Params: params, Fields: fields})
	if err != nil {
		return nil, err
	}
	url := c.URL
	if url == "" {
		url = DefaultURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tushare %s: %w", apiName, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tushare %s: read body: %w", apiName, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tushare %s: http %d: %s", apiName, resp.StatusCode, truncate(string(body), 200))
	}
	var out response
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("tushare %s: decode: %w (body=%s)", apiName, err, truncate(string(body), 200))
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("tushare %s: code=%d msg=%s", apiName, out.Code, out.Msg)
	}
	if out.Data == nil {
		return &Result{}, nil
	}
	if c.Logf != nil {
		c.Logf("tushare %s: %d rows (has_more=%v)", apiName, len(out.Data.Items), out.Data.HasMore)
	}
	if c.Pause > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.Pause):
		}
	}
	return &Result{Fields: out.Data.Fields, Items: out.Data.Items, HasMore: out.Data.HasMore}, nil
}

// CallAll 自动分页拉取全部结果。
func (c *Client) CallAll(ctx context.Context, apiName string, params map[string]any, fields string) (*Result, error) {
	limit := c.Limit
	if limit <= 0 {
		limit = 5000
	}
	all := &Result{}
	offset := 0
	for {
		p := make(map[string]any, len(params)+2)
		for k, v := range params {
			p[k] = v
		}
		p["limit"] = strconv.Itoa(limit)
		p["offset"] = strconv.Itoa(offset)
		res, err := c.Call(ctx, apiName, p, fields)
		if err != nil {
			return nil, err
		}
		if len(all.Fields) == 0 {
			all.Fields = res.Fields
		}
		all.Items = append(all.Items, res.Items...)
		if !res.HasMore || len(res.Items) == 0 {
			break
		}
		offset += len(res.Items)
	}
	return all, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
