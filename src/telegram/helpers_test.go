package telegram

import (
	"megabot/src/database"
	"strings"
	"testing"
)

func TestHistoryMessagesMergesAndTrims(t *testing.T) {
	thread := []database.Message{
		{Role: "user", Content: "domanda 1"},
		{Role: "assistant", Content: "parte A"},
		{Role: "assistant", Content: "parte B"},
		{Role: "user", Content: "domanda 2"},
		{Role: "assistant", Content: "risposta 2"},
	}
	got := historyMessages(thread, 1000)
	if len(got) != 4 || got[1].Role != "assistant" || got[1].Content != "parte A\nparte B" {
		t.Fatalf("merge: %+v", got)
	}
	// budget for only the last two turns: oldest are dropped
	got = historyMessages(thread, len("domanda 2")+len("risposta 2"))
	if len(got) != 2 || got[0].Content != "domanda 2" {
		t.Fatalf("trim: %+v", got)
	}
	// a single oversized turn is still kept (better than no context)
	got = historyMessages([]database.Message{{Role: "user", Content: strings.Repeat("x", 50)}}, 10)
	if len(got) != 1 {
		t.Fatalf("oversized: %+v", got)
	}
}

func TestBudgetError(t *testing.T) {
	if budgetError(4.99, 5, false) != nil {
		t.Fatal("under budget must pass")
	}
	err := budgetError(5, 5, true)
	if err == nil || !strings.Contains(err.Error(), "$5.00 su $5.00") || !strings.Contains(err.Error(), "/llm") {
		t.Fatalf("got %v", err)
	}
	if err := budgetError(6, 5, false); strings.Contains(err.Error(), "/llm") {
		t.Fatalf("no free alternative should not suggest /llm: %v", err)
	}
}

func TestParseBudget(t *testing.T) {
	for in, want := range map[string]float64{"5": 5, "2.5": 2.5, "2,5": 2.5, "$10": 10, "0": 0} {
		got, err := parseBudget(in)
		if err != nil || got == nil || *got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"none", "∞", "INF"} {
		if got, err := parseBudget(in); err != nil || got != nil {
			t.Errorf("%q should mean unlimited: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"-1", "tanti", ""} {
		if _, err := parseBudget(in); err == nil {
			t.Errorf("%q should fail", in)
		}
	}
}

func TestMoneyAndCommandOf(t *testing.T) {
	if money(5) != "5" || money(0.1234) != "0.12" || money(12.5) != "12.50" {
		t.Fatalf("money: %s %s %s", money(5), money(0.1234), money(12.5))
	}
	cases := map[string]string{"/login": "login", "/LOGIN@MegaBot": "login", " /start x": "start", "ciao": "", "/image-raw x": "image-raw"}
	for in, want := range cases {
		if got := commandOf(in); got != want {
			t.Errorf("commandOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatUsers(t *testing.T) {
	five := 5.0
	list := []database.UserSummary{
		{User: database.User{ID: 2, Username: "bob", Status: database.StatusPending}},
		{User: database.User{ID: 1, Name: "Dan", Username: "dan", Status: database.StatusApproved, LLM: "mimir"}, MonthUSD: 1.5, Requests: 10},
		{User: database.User{ID: 3, Name: "Eve", Status: database.StatusApproved, BudgetUSD: &five}, MonthUSD: 0.25},
	}
	got := formatUsers(list, 1, "openai")
	for _, want := range []string{"⏳ @bob", "✅ Dan @dan 👑", "mese $1.50 / ∞", "10 richieste · mimir", "mese $0.25 / $5", "· openai"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatUsage(t *testing.T) {
	rows := []database.UsageRow{
		{UserID: 1, UserName: "dan", Kind: "audio", Provider: "openai", Count: 3, MonthUSD: 0.02, TotalUSD: 0.05},
		{UserID: 1, UserName: "dan", Kind: "text", Provider: "mimir", Count: 1},
		{UserID: 2, UserName: "bob", Kind: "image", Provider: "openai", Count: 1, TotalUSD: 0.04},
	}
	got := formatUsage(rows)
	for _, want := range []string{"👤 dan · mese $0.02 · totale $0.05", "audio (openai): 3", "text (mimir): 1", "👤 bob · mese $0 · totale $0.04"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if formatUsage(nil) != "Nessuna richiesta registrata." {
		t.Fatal("empty usage")
	}
}

func TestAudioFilename(t *testing.T) {
	if audioFilename("nota.m4a", "audio/mp4") != "nota.m4a" {
		t.Fatal("keeps a real filename")
	}
	if got := audioFilename("", "audio/mpeg"); !strings.HasPrefix(got, "audio.") {
		t.Fatalf("from mime: %q", got)
	}
	if audioFilename("", "") != "audio.mp3" {
		t.Fatal("fallback")
	}
}
