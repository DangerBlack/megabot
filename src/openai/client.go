// Package openai is a minimal client for the few OpenAI endpoints the bot uses:
// chat completions (text and vision), audio transcription and image generation.
package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.openai.com/v1"

type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func New(apiKey, baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		// image generation can take well over a minute
		HTTP: &http.Client{Timeout: 4 * time.Minute},
	}
}

// ContentPart is one element of a multimodal message.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

// Message content is either a string or a []ContentPart.
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type apiError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// Error is returned for non-2xx responses.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("openai: HTTP %d: %s", e.Status, e.Message)
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var ae apiError
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &ae) == nil && ae.Error.Message != "" {
			msg = ae.Error.Message
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &Error{Status: resp.StatusCode, Message: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("openai: decoding response: %w", err)
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path, "application/json", bytes.NewReader(b), out)
}

// Result is a chat answer plus the token usage reported by the API (0 if absent).
type Result struct {
	Text      string
	TokensIn  int
	TokensOut int
}

// Chat sends a chat completion request and returns the assistant's text.
func (c *Client) Chat(ctx context.Context, model string, messages []Message) (Result, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	err := c.postJSON(ctx, "/chat/completions", map[string]any{"model": model, "messages": messages}, &resp)
	if err != nil {
		return Result{}, err
	}
	if len(resp.Choices) == 0 {
		return Result{}, fmt.Errorf("openai: empty response")
	}
	return Result{
		Text:      strings.TrimSpace(resp.Choices[0].Message.Content),
		TokensIn:  resp.Usage.PromptTokens,
		TokensOut: resp.Usage.CompletionTokens,
	}, nil
}

// Ask is a single-turn text prompt.
func (c *Client) Ask(ctx context.Context, model, prompt string) (Result, error) {
	return c.Chat(ctx, model, []Message{{Role: "user", Content: prompt}})
}

// Vision sends one image (as a data URL) together with a text prompt.
func (c *Client) Vision(ctx context.Context, model, prompt string, image []byte, mimeType string) (Result, error) {
	dataURL := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(image)
	return c.Chat(ctx, model, []Message{{
		Role: "user",
		Content: []ContentPart{
			{Type: "text", Text: prompt},
			{Type: "image_url", ImageURL: &ImageURL{URL: dataURL}},
		},
	}})
}

// Transcribe sends an audio file to the transcription endpoint.
func (c *Client) Transcribe(ctx context.Context, model, filename string, audio io.Reader) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model", model); err != nil {
		return "", err
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, audio); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	var resp struct {
		Text string `json:"text"`
	}
	if err := c.do(ctx, http.MethodPost, "/audio/transcriptions", mw.FormDataContentType(), &buf, &resp); err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text), nil
}

// GenerateImage returns the bytes of one generated image.
func (c *Client) GenerateImage(ctx context.Context, model, prompt string) ([]byte, error) {
	var resp struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	err := c.postJSON(ctx, "/images/generations", map[string]any{"model": model, "prompt": prompt, "n": 1}, &resp)
	if err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("openai: no image returned")
	}
	if b := resp.Data[0].B64JSON; b != "" {
		return base64.StdEncoding.DecodeString(b)
	}
	if u := resp.Data[0].URL; u != "" {
		return c.download(ctx, u)
	}
	return nil, fmt.Errorf("openai: image without data")
}

func (c *Client) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai: image download HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 50<<20))
}
