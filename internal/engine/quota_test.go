package engine

import (
	"strings"
	"testing"
)

func TestParseQuotaEnglish(t *testing.T) {
	text := `Settings
General
Usage
Free plan
Weekly limit resets on Sep 30
1% used
Additional tokens
Never expires
0% used (2B tokens left)
Language
88% used`
	q := ParseQuota(text)
	if !q.Found || q.Plan != "Free plan" {
		t.Fatalf("unexpected quota: %+v", q)
	}
	if q.WeeklyReset != "Sep 30" {
		t.Fatalf("weekly reset = %q", q.WeeklyReset)
	}
	if q.WeeklyUsedPct == nil || *q.WeeklyUsedPct != 1 {
		t.Fatalf("weekly pct = %v", q.WeeklyUsedPct)
	}
	if q.ExtraUsedPct == nil || *q.ExtraUsedPct != 0 {
		t.Fatalf("extra pct = %v", q.ExtraUsedPct)
	}
	if q.ExtraLeft != "2B tokens left" || q.ExtraExpires != "never" {
		t.Fatalf("extra fields: %+v", q)
	}
	if strings.Contains(q.Raw, "88%") {
		t.Fatalf("raw must exclude the language section: %q", q.Raw)
	}
}

func TestParseQuotaTraditionalChinese(t *testing.T) {
	text := "使用量\n免費計劃\n每週上限將於 10月30日 重設\n7% used\n額外憑證\n永不過期\n0% used (剩餘 20 億個憑證)\n語言\n88% used"
	q := ParseQuota(text)
	if !q.Found || q.Plan != "免費計劃" || q.WeeklyReset != "10月30日" {
		t.Fatalf("unexpected quota: %+v", q)
	}
	if q.WeeklyUsedPct == nil || *q.WeeklyUsedPct != 7 {
		t.Fatalf("weekly pct = %v", q.WeeklyUsedPct)
	}
	if q.ExtraLeft != "剩餘 20 億個憑證" || q.ExtraExpires != "never" {
		t.Fatalf("extra fields: %+v", q)
	}
}

func TestParseQuotaSimplifiedChinese(t *testing.T) {
	text := "使用量\n免费计划\n每周额度将于 10月22日 重置\n已用 12.75%\n额外令牌\n永不过期\n已使用 0%（剩余 20 亿令牌）\n语言"
	q := ParseQuota(text)
	if !q.Found || q.Plan != "免费计划" || q.WeeklyReset != "10月22日" {
		t.Fatalf("unexpected quota: %+v", q)
	}
	if q.WeeklyUsedPct == nil || *q.WeeklyUsedPct != 12.75 {
		t.Fatalf("weekly pct = %v", q.WeeklyUsedPct)
	}
	if q.ExtraLeft != "剩余 20 亿令牌" {
		t.Fatalf("extra left = %q", q.ExtraLeft)
	}
}

func TestParseQuotaPercentagesAndNegatives(t *testing.T) {
	cases := []struct {
		text  string
		found bool
		pct   float64
	}{
		{"Usage\nFree plan\n0% used", true, 0},
		{"Usage\nFree plan\n已用 0%", true, 0},
		{"Usage\nFree plan\n已使用 ０％", true, 0},
		{"Usage\nFree plan\n12.75% used", true, 12.75},
		{"Usage\nFree plan\n101% used", false, 0},
		{"Usage\nFree plan\n100% used", true, 100},
		{"Free plan", false, 0},
		{"免費計劃", false, 0},
		{"免费计划", false, 0},
		{"", false, 0},
		{"Settings\nGeneral", false, 0},
		{"Upgrade\n100% discount", false, 0},
	}
	for _, tc := range cases {
		q := ParseQuota(tc.text)
		if q.Found != tc.found {
			t.Fatalf("%q: found=%v, want %v (%+v)", tc.text, q.Found, tc.found, q)
		}
		if tc.found && (q.WeeklyUsedPct == nil || *q.WeeklyUsedPct != tc.pct) {
			t.Fatalf("%q: pct=%v, want %v", tc.text, q.WeeklyUsedPct, tc.pct)
		}
	}
}

func TestParseQuotaExtraOnlyIsNotWeeklyUsage(t *testing.T) {
	q := ParseQuota("Usage\nExtra tokens\n7.5% used (10 tokens left)")
	if q.Found || q.WeeklyUsedPct != nil {
		t.Fatalf("extra-only panel must not report weekly usage: %+v", q)
	}
	if q.ExtraUsedPct == nil || *q.ExtraUsedPct != 7.5 || q.ExtraLeft != "10 tokens left" {
		t.Fatalf("extra fields: %+v", q)
	}
}

func TestParseQuotaSectionExclusion(t *testing.T) {
	q := ParseQuota("Profile\n99% used\nUsage\nFree plan\n0% used\nLanguage\n88% used")
	if !q.Found || q.WeeklyUsedPct == nil || *q.WeeklyUsedPct != 0 {
		t.Fatalf("unexpected quota: %+v", q)
	}
	if strings.Contains(q.Raw, "99%") || strings.Contains(q.Raw, "88%") {
		t.Fatalf("raw leaked unrelated sections: %q", q.Raw)
	}
}
