// Package config reads the bot settings from the environment.
package config

import (
	"fmt"
	"strconv"
	"strings"
)

// Provider is an OpenAI-compatible endpoint used for text and vision.
type Provider struct {
	Name         string
	BaseURL      string
	APIKey       string
	ChatModel    string
	SummaryModel string
	VisionModel  string
	PriceIn      float64 // USD per 1M input tokens
	PriceOut     float64 // USD per 1M output tokens
}

type Config struct {
	Token          string
	RootUserID     int64
	DBPath         string
	HealthcheckURL string

	Providers     map[string]Provider
	ProviderOrder []string // as listed in LLM_PROVIDERS
	DefaultLLM    string

	TranscribeProvider    string // "" = disabled
	TranscribeModel       string
	TranscribePricePerMin float64
	ImageProvider         string // "" = disabled
	ImageModel            string
	ImagePrice            float64 // USD per image

	DefaultUserBudget float64 // USD/month for users approved with the default button

	SummaryMaxChars      int
	FetchFallback        string // none | jina
	JinaAPIKey           string
	HistoryMaxMessages   int
	MessageRetentionDays int
}

// OpenAI defaults (prices as of late 2026: verify on openai.com/api/pricing).
const (
	openAIBaseURL          = "https://api.openai.com/v1"
	DefaultOpenAIChat      = "gpt-5.1"
	DefaultOpenAISummary   = "gpt-5"
	DefaultOpenAIVision    = "gpt-5.1"
	DefaultOpenAIPriceIn   = 1.25
	DefaultOpenAIPriceOut  = 10.0
	DefaultTranscribeModel = "whisper-1"
	DefaultTranscribePrice = 0.006
	DefaultImageModel      = "gpt-image-1"
	DefaultImagePrice      = 0.04
	DefaultUserBudget      = 5.0
	DefaultSummaryMaxChars = 60000
	DefaultHistoryMessages = 20
	DefaultRetentionDays   = 30
)

type env struct {
	get  func(string) string
	errs []string
}

func (e *env) str(key, def string) string {
	if v := strings.TrimSpace(e.get(key)); v != "" {
		return v
	}
	return def
}

func (e *env) float(key string, def float64) float64 {
	raw := strings.TrimSpace(e.get(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		e.errs = append(e.errs, fmt.Sprintf("%s must be a non-negative number, got %q", key, raw))
		return def
	}
	return v
}

func (e *env) int(key string, def int) int {
	raw := strings.TrimSpace(e.get(key))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		e.errs = append(e.errs, fmt.Sprintf("%s must be a positive integer, got %q", key, raw))
		return def
	}
	return v
}

// Load builds a Config from getenv (os.Getenv in production, a map in tests).
func Load(getenv func(string) string) (Config, error) {
	e := &env{get: getenv}
	c := Config{
		Token:                 e.str("TOKEN", ""),
		DBPath:                e.str("DB_PATH", "./data"),
		HealthcheckURL:        e.str("HEALTHCHECK_URL", ""),
		TranscribeModel:       e.str("TRANSCRIBE_MODEL", DefaultTranscribeModel),
		TranscribePricePerMin: e.float("TRANSCRIBE_PRICE_PER_MIN", DefaultTranscribePrice),
		ImageModel:            e.str("IMAGE_MODEL", DefaultImageModel),
		ImagePrice:            e.float("IMAGE_PRICE", DefaultImagePrice),
		DefaultUserBudget:     e.float("DEFAULT_USER_BUDGET_USD", DefaultUserBudget),
		SummaryMaxChars:       e.int("SUMMARY_MAX_CHARS", DefaultSummaryMaxChars),
		FetchFallback:         strings.ToLower(e.str("FETCH_FALLBACK", "none")),
		JinaAPIKey:            e.str("JINA_API_KEY", ""),
		HistoryMaxMessages:    e.int("HISTORY_MAX_MESSAGES", DefaultHistoryMessages),
		MessageRetentionDays:  e.int("MESSAGE_RETENTION_DAYS", DefaultRetentionDays),
		Providers:             map[string]Provider{},
	}
	if c.Token == "" {
		e.errs = append(e.errs, "TOKEN is required")
	}
	if raw := e.str("ROOT_USER_ID", ""); raw == "" {
		e.errs = append(e.errs, "ROOT_USER_ID is required (your numeric Telegram user id)")
	} else if id, err := strconv.ParseInt(raw, 10, 64); err != nil {
		e.errs = append(e.errs, fmt.Sprintf("ROOT_USER_ID must be numeric, got %q", raw))
	} else {
		c.RootUserID = id
	}
	if c.FetchFallback != "none" && c.FetchFallback != "jina" {
		e.errs = append(e.errs, fmt.Sprintf("FETCH_FALLBACK must be none or jina, got %q", c.FetchFallback))
	}

	openAIKey := e.str("OPENAI_API_KEY", "")
	names := splitList(e.str("LLM_PROVIDERS", ""))
	if openAIKey != "" && !contains(names, "openai") {
		names = append(names, "openai") // always available for transcription/images
	}
	for _, name := range names {
		p := loadProvider(e, name, openAIKey)
		c.Providers[name] = p
		c.ProviderOrder = append(c.ProviderOrder, name)
	}
	if len(c.ProviderOrder) == 0 {
		e.errs = append(e.errs, "no LLM configured: set OPENAI_API_KEY and/or LLM_PROVIDERS")
	} else {
		c.DefaultLLM = strings.ToLower(e.str("DEFAULT_LLM", c.ProviderOrder[0]))
		if _, ok := c.Providers[c.DefaultLLM]; !ok {
			e.errs = append(e.errs, fmt.Sprintf("DEFAULT_LLM %q is not in LLM_PROVIDERS", c.DefaultLLM))
		}
	}

	def := ""
	if _, ok := c.Providers["openai"]; ok {
		def = "openai"
	}
	c.TranscribeProvider = media(e, c, "TRANSCRIBE_PROVIDER", def)
	c.ImageProvider = media(e, c, "IMAGE_PROVIDER", def)

	if len(e.errs) > 0 {
		return c, fmt.Errorf("config: %s", strings.Join(e.errs, "; "))
	}
	return c, nil
}

func loadProvider(e *env, name, openAIKey string) Provider {
	pre := "LLM_" + strings.ToUpper(name) + "_"
	if name == "openai" {
		return Provider{
			Name:         name,
			BaseURL:      e.str(pre+"BASE_URL", e.str("OPENAI_BASE_URL", openAIBaseURL)),
			APIKey:       e.str(pre+"API_KEY", openAIKey),
			ChatModel:    e.str(pre+"CHAT_MODEL", DefaultOpenAIChat),
			SummaryModel: e.str(pre+"SUMMARY_MODEL", DefaultOpenAISummary),
			VisionModel:  e.str(pre+"VISION_MODEL", DefaultOpenAIVision),
			PriceIn:      e.float(pre+"PRICE_IN", DefaultOpenAIPriceIn),
			PriceOut:     e.float(pre+"PRICE_OUT", DefaultOpenAIPriceOut),
		}
	}
	p := Provider{
		Name:      name,
		BaseURL:   e.str(pre+"BASE_URL", ""),
		APIKey:    e.str(pre+"API_KEY", ""),
		ChatModel: e.str(pre+"CHAT_MODEL", ""),
		PriceIn:   e.float(pre+"PRICE_IN", 0),
		PriceOut:  e.float(pre+"PRICE_OUT", 0),
	}
	p.SummaryModel = e.str(pre+"SUMMARY_MODEL", p.ChatModel)
	p.VisionModel = e.str(pre+"VISION_MODEL", p.ChatModel)
	for _, k := range []struct{ key, val string }{{"BASE_URL", p.BaseURL}, {"API_KEY", p.APIKey}, {"CHAT_MODEL", p.ChatModel}} {
		if k.val == "" {
			e.errs = append(e.errs, pre+k.key+" is required")
		}
	}
	if name == "openai" && p.APIKey == "" {
		e.errs = append(e.errs, "OPENAI_API_KEY is required for the openai provider")
	}
	return p
}

func media(e *env, c Config, key, def string) string {
	v := strings.ToLower(e.str(key, def))
	if v == "none" {
		return ""
	}
	if v != "" {
		if _, ok := c.Providers[v]; !ok {
			e.errs = append(e.errs, fmt.Sprintf("%s %q is not a configured provider", key, v))
		}
	}
	return v
}

func splitList(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" && !contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Cost returns the USD cost of a chat call on p.
func (p Provider) Cost(tokensIn, tokensOut int) float64 {
	return (float64(tokensIn)*p.PriceIn + float64(tokensOut)*p.PriceOut) / 1e6
}

// Paid reports whether calls to p cost money.
func (p Provider) Paid() bool { return p.PriceIn > 0 || p.PriceOut > 0 }
