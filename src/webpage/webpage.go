// Package webpage downloads a page and extracts its main readable text.
//
// Strategy:
//  1. plain GET with browser headers;
//  2. main content via Readability (Firefox reader mode), goquery body text as backup;
//  3. de-duplicate repeated lines (menus, cookie banners) and squeeze whitespace;
//  4. if the site blocks us or the page needs JavaScript, try the configured fallback;
//  5. cut to a budget keeping the head (70%) and the tail (30%) of the text.
package webpage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/PuerkitoBio/goquery"
)

// Browser-like headers: many sites refuse requests that look like bots.
// Accept-Encoding is left to net/http so gzip is decoded transparently.
var browserHeaders = map[string]string{
	"User-Agent":      "Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0",
	"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	"Accept-Language": "it-IT,it;q=0.8,en-US;q=0.5,en;q=0.3",
	"DNT":             "1",
}

const (
	maxBody = 5 << 20 // 5 MB
	// below this many runes we assume the page is a JS shell or a block page
	minUsefulRunes = 500
)

var urlPattern = regexp.MustCompile(`https?://[^\s<>"']+`)

// FirstURL returns the first http(s) URL in text, or "".
func FirstURL(text string) string {
	u := urlPattern.FindString(text)
	return strings.TrimRight(u, ".,;:!?)]}»")
}

// Page is the extracted content of a URL.
type Page struct {
	Title string
	Text  string
	Via   string // "direct" or the fallback name
}

// ErrBlocked means the direct fetch did not give usable text (HTTP block,
// challenge page or JavaScript-only site).
var ErrBlocked = errors.New("pagina bloccata o generata via JavaScript")

// Fallback fetches a URL some other way (rendering service, headless browser…)
// and returns its readable text.
type Fallback interface {
	Name() string
	Fetch(ctx context.Context, pageURL string) (string, error)
}

type Fetcher struct {
	Client   *http.Client
	Fallback Fallback // optional
	MaxRunes int
}

// Fetch returns the readable text of pageURL, already cut to MaxRunes.
func (f *Fetcher) Fetch(ctx context.Context, pageURL string) (Page, error) {
	page, err := f.direct(ctx, pageURL)
	if err != nil && f.Fallback != nil && errors.Is(err, ErrBlocked) {
		text, ferr := f.Fallback.Fetch(ctx, pageURL)
		if ferr != nil {
			return Page{}, fmt.Errorf("%v; fallback %s: %w", err, f.Fallback.Name(), ferr)
		}
		page = Page{Text: Clean(text), Via: f.Fallback.Name()}
		if utf8.RuneCountInString(page.Text) < 50 {
			return Page{}, fmt.Errorf("%v; anche %s non ha restituito testo", err, f.Fallback.Name())
		}
		err = nil
	}
	if err != nil {
		return Page{}, err
	}
	page.Text = HeadTail(page.Text, f.MaxRunes)
	return page, nil
}

func (f *Fetcher) direct(ctx context.Context, pageURL string) (Page, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return Page{}, err
	}
	for k, v := range browserHeaders {
		req.Header.Set(k, v)
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return Page{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 429 || resp.StatusCode == 503:
		return Page{}, fmt.Errorf("%w (HTTP %d)", ErrBlocked, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return Page{}, fmt.Errorf("la pagina ha risposto HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Page{}, err
	}

	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case mt == "text/plain" || mt == "text/markdown":
		return Page{Text: Clean(string(body)), Via: "direct"}, nil
	case mt == "application/pdf":
		return Page{}, errors.New("i PDF non sono ancora supportati")
	}

	page := Page{Via: "direct"}
	u, _ := url.Parse(pageURL)
	if art, err := readability.FromReader(bytes.NewReader(body), u); err == nil && art.Node != nil {
		var sb strings.Builder
		if art.RenderText(&sb) == nil {
			page.Text = Clean(sb.String())
		}
		page.Title = strings.TrimSpace(art.Title())
	}
	if utf8.RuneCountInString(page.Text) < minUsefulRunes {
		// Readability can be too aggressive on non-article pages: try the whole body.
		if alt, err := ExtractText(bytes.NewReader(body)); err == nil && utf8.RuneCountInString(alt) > utf8.RuneCountInString(page.Text) {
			page.Text = alt
		}
	}
	if utf8.RuneCountInString(page.Text) < minUsefulRunes || looksLikeChallenge(page.Text) {
		return page, fmt.Errorf("%w (solo %d caratteri utili)", ErrBlocked, utf8.RuneCountInString(page.Text))
	}
	return page, nil
}

var challengeMarkers = []string{
	"just a moment", "enable javascript", "attiva javascript", "abilita javascript",
	"checking your browser", "verify you are human", "cf-browser-verification", "access denied",
}

func looksLikeChallenge(text string) bool {
	if utf8.RuneCountInString(text) > 3000 {
		return false // a real article that happens to mention one of the phrases
	}
	low := strings.ToLower(text)
	for _, m := range challengeMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

var spaces = regexp.MustCompile(`[ \t\r\f\v\x{00a0}]+`)
var blankLines = regexp.MustCompile(`\n{3,}`)

// Clean squeezes whitespace and drops lines that already appeared verbatim
// (navigation, cookie banners and share widgets are usually repeated).
func Clean(text string) string {
	text = spaces.ReplaceAllString(text, " ")
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && utf8.RuneCountInString(line) >= 4 {
			if seen[line] {
				continue
			}
			seen[line] = true
		}
		out = append(out, line)
	}
	return strings.TrimSpace(blankLines.ReplaceAllString(strings.Join(out, "\n"), "\n\n"))
}

// ExtractText returns the visible text of an HTML document, preferring <article>/<main>.
// It is the backup extractor when Readability finds nothing.
func ExtractText(r io.Reader) (string, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return "", err
	}
	doc.Find("script, style, noscript, svg, iframe, template, nav, footer, header, form, aside").Remove()

	root := doc.Find("article").First()
	if root.Length() == 0 || len(strings.TrimSpace(root.Text())) < 200 {
		root = doc.Find("main").First()
	}
	if root.Length() == 0 || len(strings.TrimSpace(root.Text())) < 200 {
		root = doc.Find("body")
	}
	// put a newline after block elements so paragraphs don't run together
	root.Find("p, div, li, h1, h2, h3, h4, h5, h6, br, tr, section").Each(func(_ int, s *goquery.Selection) {
		s.AppendHtml("\n")
	})
	return Clean(root.Text()), nil
}

const cutMarker = "\n\n[…]\n\n"

// HeadTail cuts s to at most n runes keeping 70% from the start and 30% from
// the end, so both the lead and the conclusions survive.
func HeadTail(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	m := utf8.RuneCountInString(cutMarker)
	if n <= m+2 {
		return string(r[:n])
	}
	head := (n - m) * 7 / 10
	tail := n - m - head
	return string(r[:head]) + cutMarker + string(r[len(r)-tail:])
}

// Jina uses the r.jina.ai reader, which renders the page (JavaScript included)
// and returns it as markdown. The URL is sent to a third-party service.
type Jina struct {
	Client *http.Client
	APIKey string // optional, raises the rate limit
}

func (j *Jina) Name() string { return "jina" }

func (j *Jina) Fetch(ctx context.Context, pageURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://r.jina.ai/"+pageURL, nil)
	if err != nil {
		return "", err
	}
	if j.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+j.APIKey)
	}
	resp, err := j.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return string(b), err
}
