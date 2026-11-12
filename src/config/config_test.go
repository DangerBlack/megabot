package config

import (
	"math"
	"strings"
	"testing"
)

func getenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func base() map[string]string {
	return map[string]string{"TOKEN": "t", "ROOT_USER_ID": "42", "OPENAI_API_KEY": "sk"}
}

func TestOpenAIOnlyDefaults(t *testing.T) {
	c, err := Load(getenv(base()))
	if err != nil {
		t.Fatal(err)
	}
	if c.RootUserID != 42 || c.DefaultLLM != "openai" || len(c.ProviderOrder) != 1 {
		t.Fatalf("%+v", c)
	}
	p := c.Providers["openai"]
	if p.APIKey != "sk" || p.ChatModel != DefaultOpenAIChat || p.SummaryModel != DefaultOpenAISummary || p.BaseURL != openAIBaseURL {
		t.Fatalf("openai provider %+v", p)
	}
	if c.TranscribeProvider != "openai" || c.ImageProvider != "openai" {
		t.Fatalf("media providers %q %q", c.TranscribeProvider, c.ImageProvider)
	}
	if c.FetchFallback != "none" || c.DefaultUserBudget != DefaultUserBudget {
		t.Fatalf("%+v", c)
	}
}

func TestLocalPlusOpenAI(t *testing.T) {
	m := base()
	m["LLM_PROVIDERS"] = "local, openai"
	m["LLM_LOCAL_BASE_URL"] = "https://local.example/v1"
	m["LLM_LOCAL_API_KEY"] = "sk-local"
	m["LLM_LOCAL_CHAT_MODEL"] = "vllm/local"
	m["LLM_LOCAL_VISION_MODEL"] = "vllm/local-vision"
	c, err := Load(getenv(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.DefaultLLM != "local" || strings.Join(c.ProviderOrder, ",") != "local,openai" {
		t.Fatalf("order %v default %s", c.ProviderOrder, c.DefaultLLM)
	}
	mi := c.Providers["local"]
	if mi.SummaryModel != "vllm/local" || mi.VisionModel != "vllm/local-vision" || mi.Paid() {
		t.Fatalf("local %+v", mi)
	}
	if c.TranscribeProvider != "openai" {
		t.Fatalf("transcribe should stay on openai, got %q", c.TranscribeProvider)
	}
}

func TestLocalOnlyDisablesMedia(t *testing.T) {
	m := map[string]string{"TOKEN": "t", "ROOT_USER_ID": "1", "LLM_PROVIDERS": "local",
		"LLM_LOCAL_BASE_URL": "u", "LLM_LOCAL_API_KEY": "k", "LLM_LOCAL_CHAT_MODEL": "m"}
	c, err := Load(getenv(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.TranscribeProvider != "" || c.ImageProvider != "" {
		t.Fatalf("media should be disabled without openai: %q %q", c.TranscribeProvider, c.ImageProvider)
	}
}

func TestProviderCost(t *testing.T) {
	p := Provider{PriceIn: 1.25, PriceOut: 10}
	if got := p.Cost(1_000_000, 100_000); math.Abs(got-2.25) > 1e-9 {
		t.Fatalf("cost %v", got)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]func(m map[string]string){
		"TOKEN is required":         func(m map[string]string) { delete(m, "TOKEN") },
		"ROOT_USER_ID is required":  func(m map[string]string) { delete(m, "ROOT_USER_ID") },
		"ROOT_USER_ID must be":      func(m map[string]string) { m["ROOT_USER_ID"] = "@me" },
		"no LLM configured":         func(m map[string]string) { delete(m, "OPENAI_API_KEY") },
		"LLM_FOO_BASE_URL":          func(m map[string]string) { m["LLM_PROVIDERS"] = "foo" },
		"DEFAULT_LLM":               func(m map[string]string) { m["DEFAULT_LLM"] = "nope" },
		"TRANSCRIBE_PROVIDER":       func(m map[string]string) { m["TRANSCRIBE_PROVIDER"] = "nope" },
		"FETCH_FALLBACK":            func(m map[string]string) { m["FETCH_FALLBACK"] = "chrome" },
		"IMAGE_PRICE":               func(m map[string]string) { m["IMAGE_PRICE"] = "-1" },
		"HISTORY_MAX_MESSAGES must": func(m map[string]string) { m["HISTORY_MAX_MESSAGES"] = "0" },
	}
	for want, mutate := range cases {
		m := base()
		mutate(m)
		_, err := Load(getenv(m))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
}

func TestMediaCanBeDisabled(t *testing.T) {
	m := base()
	m["IMAGE_PROVIDER"] = "none"
	c, err := Load(getenv(m))
	if err != nil || c.ImageProvider != "" {
		t.Fatalf("%q %v", c.ImageProvider, err)
	}
}
