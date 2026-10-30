package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// channelURL 与桌面客户端一致：benefit 使用默认渠道，education 显式传参。
func channelURL(endpoint, channel string) string {
	if channel == MaasEducation {
		return endpoint + "?channel=" + url.QueryEscape(channel)
	}
	return endpoint
}

// fetchChannelModels 先查策略中心开关，只有 enabled=true 才领取及发现模型。
// enabled=nil 表示尚未确认，不能据此关闭账号的已知渠道或删除已有路由。
func (c *Client) fetchChannelModels(ctx context.Context, accountID string, cred SignCredential, channel string) ([]ModelInfo, *bool, error) {
	enabled, err := c.fetchGatewayEnabled(ctx, accountID, cred, channel)
	if err != nil {
		return nil, nil, err
	}
	if !enabled {
		Log("%s gateway disabled, skipping claim and models", channel)
		return nil, &enabled, nil
	}
	infos, err := c.fetchGatewayModels(ctx, accountID, cred, channel)
	return infos, &enabled, err
}

func (c *Client) fetchGatewayEnabled(ctx context.Context, accountID string, cred SignCredential, channel string) (bool, error) {
	path := EpBenefitGateway
	if channel == MaasEducation {
		path = EpEducationGateway
	}
	key := accountID + "|" + channel + "|gateway"
	if c.srcBlocked(key) {
		return false, fmt.Errorf("%s gateway backoff", channel)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.snapURL(path), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, nil, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		c.noteSrcFail(key)
		return false, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		c.noteSrcFail(key)
		return false, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.noteSrcFail(key)
		return false, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: path}
	}
	var out struct {
		Enabled *bool `json:"enabled"`
		Result  struct {
			Enabled *bool `json:"enabled"`
		} `json:"result"`
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		c.noteSrcFail(key)
		return false, fmt.Errorf("parse %s gateway: %w", channel, err)
	}
	if out.ErrorCode != "" && out.ErrorCode != "0000" && out.ErrorCode != "0" {
		c.noteSrcFail(key)
		return false, fmt.Errorf("%s gateway error_code=%s", channel, out.ErrorCode)
	}
	enabled := out.Enabled
	if enabled == nil {
		enabled = out.Result.Enabled
	}
	if enabled == nil {
		c.noteSrcFail(key)
		return false, fmt.Errorf("%s gateway response missing enabled", channel)
	}
	c.clearSrcFail(key)
	return *enabled, nil
}
