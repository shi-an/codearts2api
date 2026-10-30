// 模型目录与限时福利路由。
//
// 上游可用模型来自三路（对齐官方客户端 ModelService）：
//  1. agent-center：useragents 找 CodeAgent（alias 智能体 + show_in_ide）→ detail 的 gpts.models
//  2. 内置：GET /v1/model/builtin（Agent-Type: PromptCenter）
//  3. 限时福利（免费套餐）：GET {opengw}/api/v1/gateway/config
//
// 权益渠道的记录需追加 maas_type: benefit / education；同 ID 的普通记录独立保存。
// 渠道与参数不能交叉合并，否则可能误路由或误用另一渠道的输出预算。
//
// 目录按账号隔离：福利是按账号授予的，池里 A 账号有、B 账号没有时不允许互相污染，
// 因此 IsBenefitModel 必须带账号身份查询（见 SetAccountModels）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"codearts2api/internal/auth"
)

// CatalogTTL 账号模型目录的有效期（超过则建议重新发现）。
const CatalogTTL = time.Hour

// 福利路由头：官方 renderer 检测到 isFreeBenefit 即添加，经 kernel 转发到上游聊天请求。
const (
	HeaderMaasType = "maas_type"
	MaasBenefit    = "benefit"
	MaasEducation  = "education"
)

// ModelInfo 模型信息（与 workbuddy/trae 一致）。
type ModelInfo struct {
	// ID 是上游推理 ID，Name 是展示名，两者不能互换。
	ID                  string `json:"id"`
	Name                string `json:"name"`
	ContextWindow       int64  `json:"contextWindow,omitempty"`
	MaxTokens           int64  `json:"maxTokens,omitempty"`
	OutputContextWindow int64  `json:"outputContextWindow,omitempty"`
	// Benefit 兼容旧版字段；教育渠道也属于客户端的权益模型。
	Benefit  bool   `json:"benefit,omitempty"`
	MaasType string `json:"maas_type,omitempty"`
	Desc     string `json:"desc,omitempty"`
}

// OutputLimit 按桌面客户端顺序读取目录声明的输出预算，不推测服务端硬限制。
func (mi ModelInfo) OutputLimit() int64 {
	if mi.OutputContextWindow > 0 {
		return mi.OutputContextWindow
	}
	if mi.MaxTokens > 0 {
		return mi.MaxTokens
	}
	return 0
}

// OutputLimitSource 保留预算来源，便于区分 IDE 与桌面端读取的字段。
func (mi ModelInfo) OutputLimitSource() string {
	if mi.OutputContextWindow > 0 {
		return "output_context_window"
	}
	if mi.MaxTokens > 0 {
		return "max_tokens"
	}
	return ""
}

// ModelChannel 返回模型的权益渠道；旧版 Benefit=true 等价于 benefit。
func ModelChannel(mi ModelInfo) string {
	if mi.MaasType != "" {
		return mi.MaasType
	}
	if mi.Benefit {
		return MaasBenefit
	}
	return ""
}

// 冷启动种子：福利套餐轮换后新增的模型在首次发现前也要走福利路由，
// 少带一次头就是一次 404，宁可多带。发现结果只会在此基础上增补。
var seedBenefitModels = []string{
	"deepseek-v4-flash-0731", "deepseek-v4-pro-0813", "glm-5.3-flash",
}

// 内置模型种子（大小写归一用，不标记福利）。
var seedKnownModels = []string{
	"GLM-5.2", "glm-5.2-sft-harmony", "GLM-5.1", "GLM-4.7", "glm-5.3",
	"openpangu-2.0-pro", "openpangu-2.0-flash",
	"Qwen3-VL-235B", "Qwen3.5-397B-A17B-VL", "Qwen3.6-27B-VL",
}

var seedBenefit = func() map[string]bool {
	m := make(map[string]bool, len(seedBenefitModels))
	for _, id := range seedBenefitModels {
		m[strings.ToLower(id)] = true
	}
	return m
}()

// accountCatalog 一个账号的模型目录快照。
type accountCatalog struct {
	infos    []ModelInfo          // 保留同 ID 的各渠道记录
	models   map[string]ModelInfo // lower(id) -> 默认记录，路由与预算一起选择
	gateways map[string]bool      // 仅记录成功读取的渠道开关；失败不能当作关闭
	exactIDs map[string]string    // lower(id/name) -> 默认记录的精确推理 ID
	// 上次确认的权益记录按 ID + 渠道保存，单个渠道失败不能覆盖另一渠道。
	lastChannels map[modelCatalogKey]ModelInfo
	fetched      time.Time
}

type modelCatalogKey struct {
	id, channel string
}

func catalogKey(mi ModelInfo) modelCatalogKey {
	return modelCatalogKey{strings.ToLower(mi.ID), ModelChannel(mi)}
}

var catalog = struct {
	sync.RWMutex
	accounts map[string]*accountCatalog // accountID -> 目录
	known    map[string]string          // lower(id) -> 精确 ID（跨账号，大小写归一）
}{accounts: map[string]*accountCatalog{}, known: map[string]string{}}

func init() {
	catalog.known = make(map[string]string, len(seedKnownModels)+len(seedBenefitModels))
	for _, id := range append(append([]string{}, seedKnownModels...), seedBenefitModels...) {
		if _, ok := catalog.known[strings.ToLower(id)]; !ok {
			catalog.known[strings.ToLower(id)] = id
		}
	}
	catalog.known["glm-5.2-arkts-spark"] = "glm-5.2-sft-harmony"
}

// SetAccountModels 整体替换某账号的模型目录（不是增补：套餐轮换后旧模型不得残留）。
// 等价于「本轮福利来源也成功」，因此不保留上一轮福利记忆。
func SetAccountModels(accountID string, infos []ModelInfo) {
	setAccountModels(accountID, infos, false)
}

// setAccountModels 写入账号目录。
//
// keepBenefit=true 表示本轮福利来源失败（内置来源成功）：此时保留上一次成功的
// 福利条目，否则非种子的福利模型会瞬间丢掉 maas_type: benefit 标记，直到下一次
// 刷新成功为止——这期间它们在上游按未注册模型 404。
func setAccountModels(accountID string, infos []ModelInfo, keepBenefit bool) {
	setAccountChannelModels(accountID, infos, map[string]bool{
		MaasBenefit: keepBenefit, MaasEducation: keepBenefit,
	}, nil)
}

// setAccountChannelModels 按渠道保留临时失败的模型，成功关闭的渠道立即摘除。
func setAccountChannelModels(accountID string, infos []ModelInfo, keepChannels, gateways map[string]bool) {
	if accountID == "" {
		return
	}
	ac := &accountCatalog{
		models:       make(map[string]ModelInfo),
		gateways:     make(map[string]bool),
		exactIDs:     make(map[string]string),
		lastChannels: map[modelCatalogKey]ModelInfo{},
		fetched:      time.Now(),
	}
	catalog.Lock()
	defer catalog.Unlock()
	if catalog.known == nil {
		catalog.known = map[string]string{}
	}
	if prev := catalog.accounts[accountID]; prev != nil {
		for channel, enabled := range prev.gateways {
			ac.gateways[channel] = enabled
		}
	}
	for channel, enabled := range gateways {
		ac.gateways[channel] = enabled
	}
	var retained []ModelInfo
	if prev := catalog.accounts[accountID]; prev != nil {
		for key, mi := range prev.lastChannels {
			if keepChannels[key.channel] {
				if enabled, known := ac.gateways[key.channel]; !known || enabled {
					retained = append(retained, mi)
				}
			}
		}
	}
	ac.infos = MergeModels(infos, retained)
	for _, mi := range ac.infos {
		if ModelChannel(mi) != "" {
			ac.lastChannels[catalogKey(mi)] = mi
		}
	}
	defaults := DefaultModels(ac.infos)
	for _, mi := range defaults {
		ac.models[strings.ToLower(mi.ID)] = mi
	}
	// 展示名可作为兼容别名，但不能覆盖同一目录中已存在的真实 ID。
	for _, mi := range defaults {
		key := strings.ToLower(strings.TrimSpace(mi.Name))
		if _, actualID := ac.models[key]; key != "" && !actualID {
			ac.exactIDs[key] = mi.ID
			catalog.known[key] = mi.ID
		}
	}
	for _, mi := range defaults {
		if mi.ID != "" {
			key := strings.ToLower(mi.ID)
			ac.exactIDs[key] = mi.ID
			// 发现结果覆盖种子；小写也可能是上游要求的精确 ID。
			catalog.known[key] = mi.ID
		}
	}
	catalog.accounts[accountID] = ac
}

// CanonicalModelForAccount 优先使用本次实际账号的推理 ID，避免其他账号的目录改变大小写或别名。
func CanonicalModelForAccount(accountID, model string) string {
	fallback := CanonicalModel(model)
	catalog.RLock()
	defer catalog.RUnlock()
	if ac := catalog.accounts[accountID]; ac != nil {
		// snap-chat 等旧版别名也必须重新经过账号目录，不能把全局大小写直接发出。
		for _, candidate := range []string{model, fallback} {
			if exact, ok := ac.exactIDs[strings.ToLower(strings.TrimSpace(candidate))]; ok {
				return exact
			}
		}
	}
	return fallback
}

// AccountModels 返回某账号的模型目录快照（未发现时 ok=false）。
func AccountModels(accountID string) ([]ModelInfo, bool) {
	catalog.RLock()
	defer catalog.RUnlock()
	ac, ok := catalog.accounts[accountID]
	if !ok {
		return nil, false
	}
	return append([]ModelInfo(nil), ac.infos...), true
}

// AccountModelInfo 只读取实际账号的模型参数；其他账号和静态种子不能提供输出上限。
func AccountModelInfo(accountID, model string) (ModelInfo, bool) {
	id := CanonicalModelForAccount(accountID, model)
	catalog.RLock()
	defer catalog.RUnlock()
	if ac := catalog.accounts[accountID]; ac != nil {
		info, ok := ac.models[strings.ToLower(id)]
		return info, ok
	}
	return ModelInfo{}, false
}

// AccountCatalogStale 报告某账号目录是否需重新发现（未发现或超过 CatalogTTL）。
func AccountCatalogStale(accountID string) bool {
	catalog.RLock()
	defer catalog.RUnlock()
	ac, ok := catalog.accounts[accountID]
	return !ok || time.Since(ac.fetched) >= CatalogTTL
}

// IsBenefitModel 报告该账号下该模型是否走限时福利路由（大小写不敏感）。
//
// 判定顺序：
//  1. 该账号目录里标了福利 → true（发现结果优先于种子）
//  2. 该账号目录里有但没有标福利 → false：它已经以内置/agent-center 身份注册，
//     套餐轮换后福利模型转正就靠这条摘掉 maas_type 头
//  3. 目录里没有这个模型（尚未发现、福利来源失败或未领取）→ 回退冷启动种子，
//     少带一次头就是一次 InferHub.002002009.404，宁可多带
func IsBenefitModel(accountID, model string) bool {
	return AccountModelChannel(accountID, model) == MaasBenefit
}

// AccountModelChannel 按账号解析渠道，明确关闭的福利网关覆盖冷启动种子。
func AccountModelChannel(accountID, model string) string {
	key := strings.ToLower(CanonicalModelForAccount(accountID, model))
	if key == "" {
		return ""
	}
	catalog.RLock()
	defer catalog.RUnlock()
	if ac, ok := catalog.accounts[accountID]; ok {
		if info, known := ac.models[key]; known {
			return ModelChannel(info)
		}
		if enabled, known := ac.gateways[MaasBenefit]; known && !enabled {
			return ""
		}
	}
	if seedBenefit[key] {
		return MaasBenefit
	}
	return ""
}

// AccountModelGatewayDisabled 排除已关闭福利渠道中的历史种子；
// 同名模型若已由当前目录登记（含普通或教育渠道），仍按当前目录处理。
func AccountModelGatewayDisabled(accountID, model string) bool {
	key := strings.ToLower(CanonicalModelForAccount(accountID, model))
	catalog.RLock()
	defer catalog.RUnlock()
	ac := catalog.accounts[accountID]
	if ac == nil || !seedBenefit[key] {
		return false
	}
	if _, known := ac.models[key]; known {
		return false
	}
	enabled, known := ac.gateways[MaasBenefit]
	return known && !enabled
}

// lookupKnownModel 按小写 ID 查精确模型 ID（种子 + 已发现模型）。
func lookupKnownModel(id string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(id))
	if key == "" {
		return "", false
	}
	catalog.RLock()
	defer catalog.RUnlock()
	exact, ok := catalog.known[key]
	return exact, ok
}

// MergeModels 只在小写 ID 和渠道都相同时合并参数，保留各渠道独立记录。
// 精确 ID 沿用该渠道的首个来源；结果按 ID、默认渠道顺序稳定排序。
func MergeModels(sets ...[]ModelInfo) []ModelInfo {
	merged := map[modelCatalogKey]ModelInfo{}
	for _, set := range sets {
		for _, mi := range set {
			if mi.ID == "" {
				continue
			}
			key := catalogKey(mi)
			mi.MaasType = key.channel
			mi.Benefit = key.channel != ""
			cur, ok := merged[key]
			if !ok {
				merged[key] = mi
				continue
			}
			merged[key] = richerModel(cur, mi)
		}
	}
	out := make([]ModelInfo, 0, len(merged))
	for _, mi := range merged {
		out = append(out, mi)
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].ID), strings.ToLower(out[j].ID)
		if li != lj {
			return li < lj
		}
		ci, cj := ModelChannel(out[i]), ModelChannel(out[j])
		if pi, pj := channelPriority(ci), channelPriority(cj); pi != pj {
			return pi < pj
		}
		if ci != cj {
			return ci < cj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// DefaultModels 为只提供模型 ID 的请求选择整条记录：普通渠道优先，
// 没有普通渠道时沿用 benefit、education 顺序；不跨渠道借用参数。
func DefaultModels(infos []ModelInfo) []ModelInfo {
	var out []ModelInfo
	seen := map[string]bool{}
	for _, mi := range MergeModels(infos) {
		key := strings.ToLower(mi.ID)
		if !seen[key] {
			out = append(out, mi)
			seen[key] = true
		}
	}
	return out
}

func channelPriority(channel string) int {
	switch channel {
	case "":
		return 0
	case MaasBenefit:
		return 1
	case MaasEducation:
		return 2
	default:
		return 3
	}
}

// richerModel 仅合并同 ID、同渠道的记录，沿用优先来源的推理身份。
func richerModel(cur, next ModelInfo) ModelInfo {
	keep, drop := cur, next
	if betterModel(next, cur) {
		keep, drop = next, cur
	}
	// 参数更全不代表 ID 大小写更准确，不能覆盖高优先级来源的推理 ID。
	keep.ID = cur.ID
	if cur.Name != "" && cur.Name != cur.ID {
		keep.Name = cur.Name
	} else if keep.Name == "" {
		keep.Name = drop.Name
	}
	if keep.ContextWindow <= 0 {
		keep.ContextWindow = drop.ContextWindow
	}
	if keep.MaxTokens <= 0 {
		keep.MaxTokens = drop.MaxTokens
	}
	if keep.OutputContextWindow <= 0 {
		keep.OutputContextWindow = drop.OutputContextWindow
	}
	if keep.Desc == "" {
		keep.Desc = drop.Desc
	}
	return keep
}

// betterModel 优先保留带上下文参数的记录；参数同等时沿用前一个来源。
func betterModel(a, b ModelInfo) bool {
	return a.ContextWindow > 0 && b.ContextWindow == 0
}

type modelCatalogLoad struct {
	done chan struct{}
	err  error
}

// EnsureAccountModels 为跳过 /v1/models 的聊天请求补齐目录。
// 同一账号合并并发发现；失败退避，避免每次聊天都重复等待上游。
func (c *Client) EnsureAccountModels(ctx context.Context, acct *auth.Auth) error {
	if acct == nil {
		return fmt.Errorf("account required for model fetch")
	}
	if !AccountCatalogStale(acct.UserID) {
		return nil
	}
	key := acct.UserID + "|catalog"
	c.srcMu.Lock()
	if !AccountCatalogStale(acct.UserID) {
		c.srcMu.Unlock()
		return nil
	}
	if pending := c.modelLoads[key]; pending != nil {
		c.srcMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-pending.done:
			return pending.err
		}
	}
	if until := c.srcFail[key]; time.Now().Before(until) {
		c.srcMu.Unlock()
		return fmt.Errorf("model catalog source backoff")
	}
	if c.modelLoads == nil {
		c.modelLoads = map[string]*modelCatalogLoad{}
	}
	pending := &modelCatalogLoad{done: make(chan struct{})}
	c.modelLoads[key] = pending
	c.srcMu.Unlock()

	_, err := c.FetchModelsContext(ctx, acct)
	c.srcMu.Lock()
	pending.err = err
	if err != nil && ctx.Err() != context.Canceled {
		if c.srcFail == nil {
			c.srcFail = map[string]time.Time{}
		}
		c.srcFail[key] = time.Now().Add(srcFailCooldown)
	}
	delete(c.modelLoads, key)
	close(pending.done)
	c.srcMu.Unlock()
	return err
}

// FetchModels 拉取指定账号当前可用模型（三路合并），并写入该账号的模型目录。
//
// 任一路失败只记日志不阻断：福利接口不可用时仍能拿到内置模型。
func (c *Client) FetchModels(acct *auth.Auth) ([]ModelInfo, error) {
	return c.FetchModelsContext(context.Background(), acct)
}

// FetchModelsContext 让首次聊天的目录发现服从请求取消与总超时。
func (c *Client) FetchModelsContext(ctx context.Context, acct *auth.Auth) ([]ModelInfo, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for model fetch")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	token, ak, sk := acct.Credentials()
	cred := SignCredential{
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		SecurityToken:   token,
	}
	accountID := acct.UserID
	var sets [][]ModelInfo
	if infos, err := c.fetchAgentModels(ctx, cred); err != nil {
		Log("agent models: %v", err)
	} else {
		sets = append(sets, infos)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if infos, err := c.fetchBuiltinModels(ctx, accountID, cred); err != nil {
		Log("builtin models: %v", err)
	} else {
		sets = append(sets, infos)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keepChannels := map[string]bool{}
	gateways := map[string]bool{}
	for _, channel := range []string{MaasBenefit, MaasEducation} {
		infos, enabled, err := c.fetchChannelModels(ctx, accountID, cred, channel)
		if enabled != nil {
			gateways[channel] = *enabled
		}
		if err != nil {
			Log("%s models: %v", channel, err)
			keepChannels[channel] = true
		} else {
			sets = append(sets, infos)
		}
		if err := ctx.Err(); err != nil {
			return nil, err // 不用被取消的半份目录覆盖已有参数。
		}
	}
	out := MergeModels(sets...)
	// 即使本次只有关闭开关，也要更新目录，避免过期渠道被旧缓存复活。
	if len(out) > 0 || len(gateways) > 0 {
		setAccountChannelModels(accountID, out, keepChannels, gateways)
		out, _ = AccountModels(accountID)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// fetchAgentModels 官方 fetchAgentModels 等价实现：agent-center detail 的 gpts.models。
func (c *Client) fetchAgentModels(ctx context.Context, cred SignCredential) ([]ModelInfo, error) {
	agentID, err := c.defaultAgentID(ctx, cred)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return nil, fmt.Errorf("no default agent found")
	}
	raw, err := c.getSigned(ctx, c.snapURL(EpAgentDetail)+"?agent_id="+url.QueryEscape(agentID), cred, true)
	if err != nil {
		return nil, err
	}
	var detail struct {
		Gpts struct {
			Models []struct {
				ModelAlias string `json:"model_alias"`
				ModelName  string `json:"model_name"`
				ModelID    string `json:"model_id"`
				Params     struct {
					ContextWindow       int64  `json:"context_window"`
					InputContextWindow  int64  `json:"input_context_window"`
					OutputContextWindow int64  `json:"output_context_window"`
					MaxTokens           int64  `json:"max_tokens"`
					ModelID             string `json:"model_id"`
					ModelDesc           string `json:"model_desc"`
					IsFreeBenefit       bool   `json:"is_free_benefit"`
					MaasType            string `json:"maas_type"`
				} `json:"model_parameters"`
			} `json:"models"`
		} `json:"gpts"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	out := make([]ModelInfo, 0, len(detail.Gpts.Models))
	for _, m := range detail.Gpts.Models {
		// 与当前 Agent 客户端一致：推理参数里的 ID 优先，展示名只用于显示。
		id := firstNonEmpty(m.Params.ModelID, m.ModelAlias, m.ModelID, m.ModelName)
		if id == "" {
			continue
		}
		out = append(out, ModelInfo{
			ID:                  id,
			Name:                firstNonEmpty(m.ModelName, id),
			ContextWindow:       firstPositive(m.Params.ContextWindow, m.Params.InputContextWindow),
			MaxTokens:           m.Params.MaxTokens,
			OutputContextWindow: m.Params.OutputContextWindow,
			Desc:                m.Params.ModelDesc,
			Benefit:             m.Params.IsFreeBenefit,
			MaasType:            m.Params.MaasType,
		})
	}
	return out, nil
}

// defaultAgentID 拉取用户 agent 列表，返回 CodeAgent 的 agent_id。
// 过滤条件与官方客户端一致：agent_name=="CodeAgent" && alias.alias_zh_cn=="智能体" && show_in_ide，
// 退化为 is_primary_agent，再退化为首个。
func (c *Client) defaultAgentID(ctx context.Context, cred SignCredential) (string, error) {
	raw, err := c.getSigned(ctx, c.snapURL(EpAgentList)+"?offset=0&limit=100&is_primary_agent=true", cred, true)
	if err != nil {
		return "", err
	}
	var out struct {
		Agents []struct {
			AgentID   string `json:"agent_id"`
			AgentName string `json:"agent_name"`
			Primary   bool   `json:"is_primary_agent"`
			ShowInIDE bool   `json:"show_in_ide"`
			Alias     struct {
				ZhCN string `json:"alias_zh_cn"`
			} `json:"alias"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("parse agents: %w", err)
	}
	for _, a := range out.Agents {
		if a.AgentName == "CodeAgent" && a.Alias.ZhCN == "智能体" && a.ShowInIDE {
			return a.AgentID, nil
		}
	}
	for _, a := range out.Agents {
		if a.Primary {
			return a.AgentID, nil
		}
	}
	if len(out.Agents) > 0 {
		return out.Agents[0].AgentID, nil
	}
	return "", nil
}

// fetchBuiltinModels GET /v1/model/builtin（Agent-Type: PromptCenter）。
func (c *Client) fetchBuiltinModels(ctx context.Context, accountID string, cred SignCredential) ([]ModelInfo, error) {
	key := accountID + "|builtin"
	if c.srcBlocked(key) {
		return nil, fmt.Errorf("builtin source backoff")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.snapURL(EpModelBuiltin), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, []byte{}, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		c.noteSrcFail(key)
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		c.noteSrcFail(key)
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: EpModelBuiltin}
	}
	var out struct {
		BuiltinModels []struct {
			ModelID             string `json:"model_id"`
			ModelName           string `json:"model_name"`
			ModelDesc           string `json:"model_desc"`
			ContextWindow       int64  `json:"context_window"`
			InputContextWindow  int64  `json:"input_context_window"`
			OutputContextWindow int64  `json:"output_context_window"`
			MaxTokens           int64  `json:"max_tokens"`
			IsFreeBenefit       bool   `json:"is_free_benefit"`
			MaasType            string `json:"maas_type"`
		} `json:"builtinModels"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		c.noteSrcFail(key)
		return nil, fmt.Errorf("parse builtin models: %w", err)
	}
	c.clearSrcFail(key)
	infos := make([]ModelInfo, 0, len(out.BuiltinModels))
	for _, m := range out.BuiltinModels {
		id := firstNonEmpty(m.ModelID, m.ModelName)
		if id == "" {
			continue
		}
		infos = append(infos, ModelInfo{
			ID: id, Name: firstNonEmpty(m.ModelName, id),
			ContextWindow: firstPositive(m.ContextWindow, m.InputContextWindow),
			MaxTokens:     m.MaxTokens, OutputContextWindow: m.OutputContextWindow, Desc: m.ModelDesc,
			Benefit: m.IsFreeBenefit, MaasType: m.MaasType,
		})
	}
	return infos, nil
}

// ClaimBenefit 显式领取默认福利渠道（幂等）。自动领取仅在渠道开关启用时执行。
func (c *Client) ClaimBenefit(cred SignCredential) error {
	return c.claimChannelBenefit(context.Background(), cred, MaasBenefit)
}

func (c *Client) claimChannelBenefit(ctx context.Context, cred SignCredential, channel string) error {
	body, _ := json.Marshal(map[string]any{})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, channelURL(c.benefitURL(EpBenefitClaim), channel), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, body, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: EpBenefitClaim}
	}
	var out struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("parse claim: %w", err)
	}
	if out.ErrorCode != "0000" {
		return fmt.Errorf("claim failed: error_code=%s msg=%s", out.ErrorCode, out.ErrorMsg)
	}
	return nil
}

// fetchGatewayModels 拉取已启用渠道的模型；开关检查由 fetchChannelModels 完成。
func (c *Client) fetchGatewayModels(ctx context.Context, accountID string, cred SignCredential, channel string) ([]ModelInfo, error) {
	key := accountID + "|" + channel
	if c.srcBlocked(key) {
		return nil, fmt.Errorf("%s source backoff", channel)
	}
	if c.claimAuto.Load() {
		if err := c.claimChannelBenefit(ctx, cred, channel); err != nil {
			Log("%s claim: %v", channel, err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, channelURL(c.benefitURL(EpBenefitConfig), channel), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Language", "zh-cn")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, []byte{}, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		c.noteSrcFail(key)
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		c.noteSrcFail(key)
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: EpBenefitConfig}
	}
	var out struct {
		ErrorCode string `json:"error_code"`
		Result    struct {
			BaseURL string `json:"base_url"`
			Models  []struct {
				ModelID             string `json:"model_id"`
				ModelIDCamel        string `json:"modelId"`
				ModelName           string `json:"model_name"`
				ModelNameCamel      string `json:"modelName"`
				ModelDesc           string `json:"model_desc"`
				Description         string `json:"description"`
				ContextWindow       int64  `json:"context_window"`
				InputContextWindow  int64  `json:"input_context_window"`
				OutputContextWindow int64  `json:"output_context_window"`
				MaxTokens           int64  `json:"max_tokens"`
			} `json:"models"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		c.noteSrcFail(key)
		return nil, fmt.Errorf("parse benefit config: %w", err)
	}
	if out.ErrorCode != "0000" {
		c.noteSrcFail(key)
		return nil, fmt.Errorf("gateway/config error_code=%s", out.ErrorCode)
	}
	if out.Result.Models == nil {
		c.noteSrcFail(key)
		return nil, fmt.Errorf("gateway/config response missing models array")
	}
	c.clearSrcFail(key)
	infos := make([]ModelInfo, 0, len(out.Result.Models))
	for _, m := range out.Result.Models {
		id := firstNonEmpty(m.ModelID, m.ModelIDCamel, m.ModelName, m.ModelNameCamel)
		if id == "" {
			continue
		}
		infos = append(infos, ModelInfo{
			ID: id, Name: firstNonEmpty(m.ModelName, m.ModelNameCamel, id),
			ContextWindow: firstPositive(m.ContextWindow, m.InputContextWindow),
			MaxTokens:     m.MaxTokens, OutputContextWindow: m.OutputContextWindow, Desc: firstNonEmpty(m.ModelDesc, m.Description),
			Benefit: true, MaasType: channel,
		})
	}
	return infos, nil
}

func firstPositive(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
