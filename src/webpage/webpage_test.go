package webpage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

var para = strings.Repeat("Questo è un paragrafo dell'articolo con contenuto vero e proprio. ", 12)

var articlePage = `<html><head><title>Titolo pagina</title><style>.x{}</style><script>var a=1;</script></head>
<body><nav>Menu Home Contatti</nav><div class="cookie">Accetta i cookie per continuare</div>
<article><h1>Titolo articolo</h1><p>` + para + `</p><p>` + para + `</p><p>Conclusione finale.</p></article>
<footer>Copyright 2026</footer><div class="cookie">Accetta i cookie per continuare</div></body></html>`

func serve(t *testing.T, status int, ctype, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("User-Agent"), "Firefox") {
			t.Errorf("missing browser UA")
		}
		w.Header().Set("Content-Type", ctype)
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type fakeFallback struct {
	text  string
	err   error
	calls int
}

func (f *fakeFallback) Name() string { return "fake" }
func (f *fakeFallback) Fetch(ctx context.Context, u string) (string, error) {
	f.calls++
	return f.text, f.err
}

func TestFetchExtractsArticleWithReadability(t *testing.T) {
	srv := serve(t, 200, "text/html; charset=utf-8", articlePage)
	f := &Fetcher{Client: srv.Client(), MaxRunes: 100000}
	p, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"var a=1", ".x{}", "Menu Home", "Copyright", "cookie"} {
		if strings.Contains(p.Text, bad) {
			t.Errorf("noise %q left in text", bad)
		}
	}
	if !strings.Contains(p.Text, "Conclusione finale.") || p.Via != "direct" {
		t.Fatalf("unexpected page %+v", p)
	}
	if p.Title == "" {
		t.Fatalf("missing title")
	}
}

func TestFetchBlockedUsesFallback(t *testing.T) {
	srv := serve(t, 403, "text/html", "<html><body>Forbidden</body></html>")
	fb := &fakeFallback{text: "# Titolo\n\n" + para}
	f := &Fetcher{Client: srv.Client(), Fallback: fb, MaxRunes: 100000}
	p, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if fb.calls != 1 || p.Via != "fake" || !strings.Contains(p.Text, "paragrafo") {
		t.Fatalf("fallback not used: %+v calls=%d", p, fb.calls)
	}
}

func TestFetchJSShellIsBlocked(t *testing.T) {
	srv := serve(t, 200, "text/html", `<html><body><div id="root"></div><noscript>Enable JavaScript to run this app.</noscript></body></html>`)
	f := &Fetcher{Client: srv.Client(), MaxRunes: 1000}
	if _, err := f.Fetch(context.Background(), srv.URL); !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected ErrBlocked, got %v", err)
	}
}

func TestFetchChallengePageIsBlocked(t *testing.T) {
	body := "<html><body><h1>Just a moment...</h1><p>" + strings.Repeat("Checking your browser before accessing the site. ", 12) + "</p></body></html>"
	srv := serve(t, 200, "text/html", body)
	fb := &fakeFallback{err: errors.New("down")}
	f := &Fetcher{Client: srv.Client(), Fallback: fb, MaxRunes: 1000}
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil || fb.calls != 1 || !strings.Contains(err.Error(), "fallback fake") {
		t.Fatalf("expected fallback attempt and error, got %v (calls %d)", err, fb.calls)
	}
}

func TestFetchNotFoundDoesNotUseFallback(t *testing.T) {
	srv := serve(t, 404, "text/html", "nope")
	fb := &fakeFallback{text: para}
	f := &Fetcher{Client: srv.Client(), Fallback: fb, MaxRunes: 1000}
	if _, err := f.Fetch(context.Background(), srv.URL); err == nil || fb.calls != 0 {
		t.Fatalf("404 should fail without fallback: %v calls=%d", err, fb.calls)
	}
}

func TestFetchPlainText(t *testing.T) {
	srv := serve(t, 200, "text/plain", para)
	p, err := (&Fetcher{Client: srv.Client(), MaxRunes: 100000}).Fetch(context.Background(), srv.URL)
	if err != nil || !strings.HasPrefix(p.Text, "Questo è") {
		t.Fatalf("got %+v %v", p, err)
	}
}

func TestFetchAppliesBudget(t *testing.T) {
	srv := serve(t, 200, "text/html", articlePage)
	p, err := (&Fetcher{Client: srv.Client(), MaxRunes: 300}).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(p.Text); n > 300 {
		t.Fatalf("text has %d runes", n)
	}
	if !strings.Contains(p.Text, "[…]") || !strings.HasSuffix(p.Text, "Conclusione finale.") {
		t.Fatalf("head/tail cut wrong: %q", p.Text)
	}
}

func TestCleanDedupesAndSqueezes(t *testing.T) {
	in := "Menu  principale\n\n\n\nTesto  uno\nMenu principale\nok\nok"
	got := Clean(in)
	if strings.Count(got, "Menu principale") != 1 {
		t.Fatalf("duplicate line kept: %q", got)
	}
	if strings.Count(got, "ok") != 2 {
		t.Fatalf("short lines must not be deduped: %q", got)
	}
	if strings.Contains(got, "\n\n\n") || strings.Contains(got, "  ") {
		t.Fatalf("whitespace not squeezed: %q", got)
	}
}

func TestHeadTail(t *testing.T) {
	s := strings.Repeat("a", 100) + strings.Repeat("z", 100)
	got := HeadTail(s, 60)
	if utf8.RuneCountInString(got) > 60 || !strings.HasPrefix(got, "aaa") || !strings.HasSuffix(got, "zzz") {
		t.Fatalf("got %q", got)
	}
	if HeadTail("corto", 60) != "corto" {
		t.Fatal("short text must be unchanged")
	}
}

func TestFirstURL(t *testing.T) {
	cases := map[string]string{
		"guarda https://example.com/a?b=1, interessante": "https://example.com/a?b=1",
		"(http://x.it/pagina).":                          "http://x.it/pagina",
		"nessun link":                                    "",
		"due https://a.it e https://b.it":                "https://a.it",
	}
	for in, want := range cases {
		if got := FirstURL(in); got != want {
			t.Errorf("FirstURL(%q) = %q, want %q", in, got, want)
		}
	}
}
