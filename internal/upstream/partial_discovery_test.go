package upstream

import "testing"

// 福利来源临时失败时，不能让已发现的非种子福利模型丢掉 maas_type: benefit
// （丢了上游就按未注册模型 404）；模型真正转正后仍要能摘掉标记。
func TestPartialDiscoveryKeepsBenefit(t *testing.T) {
	acct := "acct-partial"
	SetAccountModels(acct, []ModelInfo{
		{ID: "brand-new-benefit-0915", Benefit: true},
		{ID: "GLM-5.2", ContextWindow: 202752},
	})
	if !IsBenefitModel(acct, "brand-new-benefit-0915") {
		t.Fatal("首次发现后应走福利路由")
	}
	// 福利来源失败 + 内置来源成功（keepBenefit=true）
	setAccountModels(acct, []ModelInfo{{ID: "GLM-5.2", ContextWindow: 202752}}, true)
	if !IsBenefitModel(acct, "brand-new-benefit-0915") {
		t.Error("福利来源失败后应保留福利标记")
	}
	// 福利来源恢复且模型被内置来源登记 → 转正，摘掉标记
	setAccountModels(acct, []ModelInfo{{ID: "brand-new-benefit-0915"}, {ID: "GLM-5.2"}}, false)
	if IsBenefitModel(acct, "brand-new-benefit-0915") {
		t.Error("模型转正后应摘掉福利标记")
	}
}

func TestPartialDiscoveryKeepsSameIDChannelsSeparate(t *testing.T) {
	const model = "partial-channel-shared"
	acct := t.Name()
	SetAccountModels(acct, []ModelInfo{
		{ID: model, MaasType: MaasBenefit, MaxTokens: 8192},
		{ID: model, MaasType: MaasEducation, OutputContextWindow: 32768},
	})
	// 两路暂时失败：同时保留，不能用同一个 ID 键覆盖其中一条。
	setAccountChannelModels(acct, nil, map[string]bool{MaasBenefit: true, MaasEducation: true}, nil)
	infos, _ := AccountModels(acct)
	if len(infos) != 2 || infos[0].OutputLimit() != 8192 || infos[1].OutputLimit() != 32768 {
		t.Fatalf("failed channels overwrote each other: %+v", infos)
	}
	// 普通模型出现时优先选普通记录，未知预算不得从历史权益记录补齐。
	setAccountChannelModels(acct, []ModelInfo{{ID: model}}, map[string]bool{MaasBenefit: true, MaasEducation: true}, nil)
	info, ok := AccountModelInfo(acct, model)
	if !ok || ModelChannel(info) != "" || info.OutputLimit() != 0 {
		t.Fatalf("cached entitlement metadata leaked into ordinary record: %+v", info)
	}
	// 福利关闭只移除福利；随后失败也不能把关闭渠道复活。
	for i := 0; i < 2; i++ {
		setAccountChannelModels(acct, nil, map[string]bool{MaasBenefit: true, MaasEducation: true}, map[string]bool{MaasBenefit: false})
		info, _ = AccountModelInfo(acct, model)
		infos, _ = AccountModels(acct)
		if len(infos) != 1 || ModelChannel(info) != MaasEducation || info.OutputLimit() != 32768 {
			t.Fatalf("closed benefit displaced education record: %+v", infos)
		}
	}
	// 教育来源成功返回空目录后，下一轮临时失败不得复活已删除的条目。
	setAccountChannelModels(acct, nil, nil, nil)
	setAccountChannelModels(acct, nil, map[string]bool{MaasEducation: true}, nil)
	if infos, _ := AccountModels(acct); len(infos) != 0 {
		t.Fatalf("removed channel entries resurrected: %+v", infos)
	}
}
