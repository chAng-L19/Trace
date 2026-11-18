package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestEvidenceChainSearchMultilingualTermsTitleWeightAndStableOrder(t *testing.T) {
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	entries := []EvidenceChainEntry{
		{RunID: "z", Chain: EvidenceChain{ID: "body", Name: "ordinary", Summary: "SQL 登录认证", CreatedAt: at.Add(time.Hour)}},
		{RunID: "b", Chain: EvidenceChain{ID: "title-b", Name: "SQL 登录认证", CreatedAt: at}},
		{RunID: "a", Chain: EvidenceChain{ID: "title-z", Name: "SQL 登录认证", CreatedAt: at}},
		{RunID: "a", Chain: EvidenceChain{ID: "title-a", Name: "SQL 登录认证", CreatedAt: at}},
		{RunID: "x", Chain: EvidenceChain{ID: "noise", Name: "unrelated", MethodID: "sql", Steps: []EvidenceChainStep{{EvidenceID: "登录"}}, CreatedAt: at}},
	}
	first := rankedEvidenceChains(entries, "登录 SQL")
	second := rankedEvidenceChains(entries, "录登 sql SQL")
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("term order/repetition changed ranking: %+v %+v", first, second)
	}
	want := []string{"title-a", "title-z", "title-b", "body"}
	if len(first) != len(want) {
		t.Fatalf("matched non-summary identity or missed terms: %+v", first)
	}
	for i, id := range want {
		if first[i].ChainID != id {
			t.Fatalf("title weight / tie-break: %+v", first)
		}
	}
	if got := rankedEvidenceChains(entries, " 	"); got[0].ChainID != "body" {
		t.Fatalf("empty query lost newest-first compatibility: %+v", got)
	}
	if got := rankedEvidenceChains(entries, "!!!"); len(got) != 0 {
		t.Fatalf("nonempty tokenless query matched all: %+v", got)
	}
	entries[0].Chain.Applicability = "Windows rollback"
	entries[0].Chain.Lesson = "timeout diagnosis"
	entries[0].Chain.Variant = "second approach"
	if got := rankedEvidenceChains(entries, "rollback diagnosis second"); len(got) != 1 || got[0].ChainID != "body" {
		t.Fatalf("method metadata not searchable: %+v", got)
	}
}

func TestEvidenceChainSearchExactAndPhrasePriority(t *testing.T) {
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	entries := []EvidenceChainEntry{
		{RunID: "source", Chain: EvidenceChain{ID: "partial", Name: "SQL only", CreatedAt: at.Add(4 * time.Hour)}},
		{RunID: "source", Chain: EvidenceChain{ID: "terms", Name: "登录 SQL analysis", CreatedAt: at.Add(3 * time.Hour)}},
		{RunID: "source", Chain: EvidenceChain{ID: "metadata-phrase", Name: "ordinary", Summary: "SQL 登录", CreatedAt: at.Add(2 * time.Hour)}},
		{RunID: "source", Chain: EvidenceChain{ID: "name-phrase", Name: "audit SQL 登录 flow", CreatedAt: at.Add(time.Hour)}},
		{RunID: "source", Chain: EvidenceChain{ID: "exact", Name: "SQL 登录", CreatedAt: at}},
	}
	hits := rankedEvidenceChains(entries, "  sQl 登录  ")
	want := []string{"exact", "name-phrase", "metadata-phrase", "terms", "partial"}
	if len(hits) != len(want) {
		t.Fatalf("exact/phrase search lost term matches: %+v", hits)
	}
	for i, id := range want {
		if hits[i].ChainID != id {
			t.Fatalf("exact/phrase priority lost to recency or term weight: %+v", hits)
		}
	}
	symbols := []EvidenceChainEntry{{RunID: "source", Chain: EvidenceChain{ID: "symbols", Name: "++"}}}
	if got := rankedEvidenceChains(symbols, "++"); len(got) != 1 || got[0].ChainID != "symbols" {
		t.Fatalf("exact symbol-only name lost to token filtering: %+v", got)
	}
	if got := rankedEvidenceChains(symbols, "!!!"); len(got) != 0 {
		t.Fatalf("unmatched symbol-only query listed unrelated names: %+v", got)
	}
}

func TestEvidenceChainSearchUnicodePreviewsKeepFullIdentityAndRead(t *testing.T) {
	_, store, source, _ := catalogFixture(t)
	entry, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: "unicode " + strings.Repeat("𠮷😀界", 100), Summary: strings.Repeat("说明😀𠮷", 300), Variant: strings.Repeat("变体😀", 100), MethodID: strings.Repeat("method-身份😀", 100), Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}})
	if err != nil {
		t.Fatal(err)
	}
	hits, total, err := store.SearchEvidenceChains("unicode", 1)
	if err != nil || total != 1 || len(hits) != 1 {
		t.Fatalf("unicode search: %+v %d %v", hits, total, err)
	}
	hit := hits[0]
	for _, field := range []struct {
		text  string
		limit int
	}{{hit.Name, 120}, {hit.Summary, 400}, {hit.Variant, 120}} {
		if !utf8.ValidString(field.text) || utf8.RuneCountInString(field.text) != field.limit || !strings.HasSuffix(field.text, "…") {
			t.Fatalf("invalid or unbounded Unicode preview: %q", field.text)
		}
	}
	if hit.SourceRunID != entry.RunID || hit.ChainID != entry.Chain.ID || hit.ChainHash != entry.Chain.ContentHash || hit.MethodID != entry.Chain.MethodID {
		t.Fatal("compact hit changed pinned identity")
	}
	full, err := store.GetEvidenceChain(hit.SourceRunID, hit.ChainID)
	if err != nil || full.Chain.ContentHash != hit.ChainHash || full.Chain.Name != entry.Chain.Name || full.Chain.Summary != entry.Chain.Summary || full.Chain.Variant != entry.Chain.Variant || len(full.Operations) != 1 {
		t.Fatalf("preview changed stored chain or full read: %v", err)
	}
	identity := strings.Repeat("identity-身份😀", 100)
	unchanged := chainSearchHit(EvidenceChainEntry{RunID: identity, Chain: EvidenceChain{ID: identity, ContentHash: identity, MethodID: identity, Name: strings.Repeat("😀", 120), Summary: strings.Repeat("界", 400), Variant: strings.Repeat("𠮷", 120)}})
	if unchanged.SourceRunID != identity || unchanged.ChainID != identity || unchanged.ChainHash != identity || unchanged.MethodID != identity || strings.Contains(unchanged.Name+unchanged.Summary+unchanged.Variant, "…") {
		t.Fatal("identity or exact-limit fields were truncated")
	}
}

func TestEvidenceChainSearchCompactHitsAndLimits(t *testing.T) {
	_, store, source, _ := catalogFixture(t)
	for i := 0; i < 12; i++ {
		_, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: fmt.Sprintf("rankable %02d", i), Summary: "retained summary", Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	hits, total, err := store.SearchEvidenceChains("rankable", 0)
	if err != nil || total != 12 || len(hits) != 10 {
		t.Fatalf("default limit: %d %+v %v", total, hits, err)
	}
	if hits[0].Name != "rankable 11" || hits[0].SourceRunID != source.Run.ID || hits[0].ChainID == "" || hits[0].ChainHash == "" || hits[0].Verification != "verified" || hits[0].StepCount != 1 {
		t.Fatalf("compact source identity lost: %+v", hits[0])
	}
	raw, _ := json.Marshal(hits)
	for _, forbidden := range []string{"\"steps\"", "derived_from", "operations", "stdout", "evidence_id"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("search leaked detail %s: %s", forbidden, raw)
		}
	}
	for _, limit := range []int{-1, 51} {
		if _, _, err := store.SearchEvidenceChains("", limit); err == nil {
			t.Fatalf("accepted invalid limit %d", limit)
		}
	}
	hits, total, err = store.SearchEvidenceChains("no-such-term", 1)
	if err != nil || total != 0 || len(hits) != 0 {
		t.Fatalf("empty matches: %+v %d %v", hits, total, err)
	}
}
