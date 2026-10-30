package server

import (
	"context"
	"log"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// ModelCatalogClient 是模型目录发现依赖，不负责发送推理请求。
type ModelCatalogClient interface {
	FetchModels(*auth.Auth) ([]upstream.ModelInfo, error)
	EnsureAccountModels(context.Context, *auth.Auth) error
}

const requestModelTimeout = 10 * time.Second

// outputTokensForAccount 仅按当前账号所选渠道声明的输出预算收窄请求。
// 不用上下文长度或模型厂商直连 API 的规格代替 CodeArts 渠道配置。
func (h *Handler) outputTokensForAccount(ctx context.Context, acct *pool.Account, model string, requested *int) *int {
	if requested == nil {
		return nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, requestModelTimeout)
	err := h.cfg.Upstream.EnsureAccountModels(lookupCtx, acct.Auth)
	cancel()
	if err != nil && ctx.Err() == nil {
		log.Printf("chat model discovery account=%s: %v", acct.Name, err)
	}
	info, ok := upstream.AccountModelInfo(acct.UID, model)
	limit := info.OutputLimit()
	if !ok || limit <= 0 || int64(*requested) <= limit {
		return requested
	}
	// 新建值，换账号重试时仍须从客户端原始预算计算，不能沿用前一账号的上限。
	effective := int(limit)
	log.Printf("max_tokens capped account=%s requested_model=%q resolved_model=%q channel=%q limit_source=%q requested_max_tokens=%d effective_max_tokens=%d",
		acct.Name, model, info.ID, upstream.ModelChannel(info), info.OutputLimitSource(), *requested, effective)
	return &effective
}
