package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New("sk-test", srv.URL)
}

func TestChatSendsModelAndReturnsContent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Fatalf("bad auth header %q", got)
		}
		var body struct {
			Model    string    `json:"model"`
			Messages []Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "gpt-test" || body.Messages[0].Content != "ciao" {
			t.Fatalf("unexpected body %+v", body)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"  risposta  "}}],"usage":{"prompt_tokens":12,"completion_tokens":34}}`))
	})
	got, err := c.Ask(context.Background(), "gpt-test", "ciao")
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "risposta" || got.TokensIn != 12 || got.TokensOut != 34 {
		t.Fatalf("got %+v", got)
	}
}

func TestVisionSendsDataURL(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), `"url":"data:image/jpeg;base64,`+base64.StdEncoding.EncodeToString([]byte("JPEG"))+`"`) {
			t.Fatalf("data url missing in %s", raw)
		}
		if !strings.Contains(string(raw), `"type":"image_url"`) || !strings.Contains(string(raw), `"text":"descrivi"`) {
			t.Fatalf("parts missing in %s", raw)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})
	if _, err := c.Vision(context.Background(), "m", "descrivi", []byte("JPEG"), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
}

func TestTranscribeMultipart(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("model") != "whisper-1" {
			t.Fatalf("model %q", r.FormValue("model"))
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(f)
		if hdr.Filename != "voice.ogg" || string(data) != "OGGDATA" {
			t.Fatalf("file %s %q", hdr.Filename, data)
		}
		w.Write([]byte(`{"text":"testo trascritto"}`))
	})
	got, err := c.Transcribe(context.Background(), "whisper-1", "voice.ogg", strings.NewReader("OGGDATA"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "testo trascritto" {
		t.Fatalf("got %q", got)
	}
}

func TestGenerateImageB64(t *testing.T) {
	png := []byte("\x89PNGfake")
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(png)}}})
	})
	got, err := c.GenerateImage(context.Background(), "gpt-image-1", "un gatto")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(png) {
		t.Fatalf("got %q", got)
	}
}

func TestGenerateImageURLFallback(t *testing.T) {
	var srvURL string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/img.png" {
			w.Write([]byte("IMG"))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"url": srvURL + "/img.png"}}})
	})
	srvURL = c.BaseURL
	got, err := c.GenerateImage(context.Background(), "dall-e-3", "x")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "IMG" {
		t.Fatalf("got %q", got)
	}
}

func TestAPIErrorMessage(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"Rate limit reached","type":"requests"}}`))
	})
	_, err := c.Ask(context.Background(), "m", "x")
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T %v", err, err)
	}
	if oe.Status != 429 || oe.Message != "Rate limit reached" {
		t.Fatalf("got %+v", oe)
	}
}
