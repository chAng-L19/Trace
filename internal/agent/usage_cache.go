package agent

// Price buckets are mutually exclusive. Both cache counts must be explicitly
// reported, including zero; absent cache-write does not mean a cache miss.
func billingTokenBuckets(u *TokenUsage) (*BillingTokenBuckets, string) {
	if u == nil || u.InputTokens == nil || u.OutputTokens == nil || *u.OutputTokens < 0 {
		return nil, "missing_or_invalid_tokens"
	}
	var cache PromptCacheStats
	cache.add(u)
	if cache.InvalidResponses > 0 {
		return nil, "conflicting_cache_tokens"
	}
	if cache.KnownReadTokens == nil || cache.KnownWriteTokens == nil {
		return nil, "missing_cache_tokens"
	}
	return &BillingTokenBuckets{Input: *u.InputTokens - *cache.KnownReadTokens - *cache.KnownWriteTokens, CacheRead: *cache.KnownReadTokens, CacheWrite: *cache.KnownWriteTokens, Output: *u.OutputTokens}, ""
}

// PromptCacheStats measures only responses with a reported, internally
// consistent cache-read count AND total prompt count. Missing is not zero.
// InputTokens retains its original inclusive meaning for budgets/calibration.
type PromptCacheStats struct {
	KnownReadTokens     *int64   `json:"known_read_tokens"`
	KnownWriteTokens    *int64   `json:"known_write_tokens"`
	ObservedInputTokens *int64   `json:"observed_input_tokens"`
	ObservedResponses   int      `json:"observed_responses"`
	MissingResponses    int      `json:"missing_responses"`
	InvalidResponses    int      `json:"invalid_responses"`
	ReadRatio           *float64 `json:"read_ratio"`
}

// References: pi openai-completions normalizeUsage; Muteki core/usage.py.
// Keep provider fields intact in the durable response; normalize only this
// read projection. Conflicting provider aliases are unknown, not a fake hit.
func (s *PromptCacheStats) add(u *TokenUsage) {
	if u == nil {
		s.MissingResponses++
		return
	}
	reads := []*int64{u.PromptCacheHitTokens, u.CachedTokens}
	var write *int64
	if u.PromptTokensDetails != nil {
		reads = append(reads, u.PromptTokensDetails.CachedTokens)
		write = u.PromptTokensDetails.CacheWriteTokens
	}
	var read *int64
	invalid := false
	for _, n := range reads {
		if n == nil {
			continue
		}
		if *n < 0 || read != nil && *read != *n {
			invalid = true
		}
		read = n
	}
	input, miss := u.InputTokens, u.PromptCacheMissTokens
	if input != nil && *input < 0 || miss != nil && *miss < 0 || write != nil && *write < 0 {
		invalid = true
	}
	if input != nil && *input >= 0 {
		if read != nil && *read > *input || miss != nil && *miss > *input || write != nil && *write > *input {
			invalid = true
		}
		if read != nil && miss != nil && *read != *input-*miss {
			invalid = true
		}
		if read != nil && write != nil && *write > *input-*read {
			invalid = true
		}
	}
	if invalid {
		s.InvalidResponses++
		return
	}
	sumToken(&s.KnownWriteTokens, write)
	if read == nil || input == nil {
		s.MissingResponses++
		return
	}
	s.ObservedResponses++
	sumToken(&s.KnownReadTokens, read)
	sumToken(&s.ObservedInputTokens, input)
	if *s.ObservedInputTokens > 0 {
		ratio := float64(*s.KnownReadTokens) / float64(*s.ObservedInputTokens)
		s.ReadRatio = &ratio
	}
}
