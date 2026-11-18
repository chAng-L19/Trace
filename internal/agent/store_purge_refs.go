package agent

func runPurgeDependency(s Snapshot, removed map[string]bool) string {
	if removed[s.Run.ParentRunID] {
		return "父会话"
	}
	for _, w := range s.Workers {
		if removed[w.RunID] {
			return "Worker " + w.ID
		}
	}
	for _, a := range s.Assets {
		if removed[a.SourceRunID] {
			return "资产 " + a.ID
		}
	}
	for _, t := range s.Plan.Tasks {
		if removed[t.ChildRunID] || removed[t.EvidenceRunID] {
			return "计划 " + t.ID
		}
	}
	for _, r := range s.EvidenceChainRefs {
		if removed[r.SourceRunID] {
			return "整链引用 " + r.ChainID
		}
	}
	for _, c := range s.EvidenceChains {
		for _, r := range c.DerivedFrom {
			if removed[r.SourceRunID] {
				return "方法经验来源 " + c.Name
			}
		}
		for _, step := range c.Steps {
			if removed[step.RunID] {
				return "操作证据链 " + c.Name
			}
		}
	}
	if c := s.WorkerContext; c != nil {
		if removed[c.ParentRunID] {
			return "Worker 上下文"
		}
		for _, e := range c.Evidence {
			if removed[e.SourceRunID] {
				return "Worker 证据来源"
			}
		}
		for _, d := range c.Dependencies {
			if removed[d.SourceRunID] {
				return "Worker 依赖来源"
			}
		}
		for _, a := range c.Assets {
			if a.Asset != nil && removed[a.Asset.SourceRunID] {
				return "Worker 资产来源"
			}
		}
	}
	inputs := func(items []InputAttachment) bool {
		for _, a := range items {
			if removed[a.SourceRunID] {
				return true
			}
		}
		return false
	}
	for _, m := range s.Messages {
		if inputs(m.Attachments) {
			return "消息附件来源"
		}
	}
	for _, q := range s.Mailbox {
		if inputs(q.Attachments) {
			return "排队附件来源"
		}
	}
	for _, request := range s.Requests {
		for _, m := range request.Messages {
			if inputs(m.Attachments) {
				return "请求附件来源"
			}
		}
	}
	return ""
}
