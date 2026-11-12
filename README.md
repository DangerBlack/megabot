# megabot

A self-hosted personal assistant for Telegram, backed by any OpenAI-compatible API: OpenAI itself, or your own server (vLLM, Ollama, LM Studio, LiteLLM …). One small Go binary, one SQLite file.

| Send the bot | What it does |
|---|---|
| 🎙 a voice note or audio file | transcribes it; long transcripts get a **📝 Riassumi** (summarize) button |
| 🔗 a message with a link | summarizes the page; **reply to the summary** to ask questions about it |
| 📷 a photo without caption | transcribes the text in the photo (OCR) |
| 📷 a photo with a caption | answers the caption about the photo ("what's the total?", "translate this") |
| `/image <prompt>` · `/image_raw <prompt>` | generates an image, as a photo or as a PNG file |
| `/gpt <question>` (in private chats just write) | answers in text |
| ↩️ a reply to one of its messages | continues the conversation, also in groups |
| `/llm [name]` | shows or switches the model provider used for text and photos |
| `/usage` | requests and spending this month |

> The bot talks to users in **Italian**.

## Access control
- `ROOT_USER_ID` is the administrator: full access, no budget.
- Anyone else sends **`/login`**; the administrator gets the request with **✅ Accept** (monthly budget `DEFAULT_USER_BUDGET_USD`), **♾ No limit** and **❌ Reject** buttons.
- Admin commands: `/users`, `/revoke <id>`, `/budget <id> <usd|none>`.
- Unknown users only get a hint to use `/login`; rejected users are ignored; in groups the bot stays silent unless an approved user talks to it.

## Costs and budgets
Every request is logged with provider, model, tokens, audio seconds and generated images, plus an **estimated cost** computed from the prices in `.env` (the defaults are estimates: check your provider's price list). When a user exceeds the monthly budget, paid features stop; providers priced at 0 (e.g. your own server) keep working, and the bot suggests `/llm`.

## Web page summaries
1. GET with browser-like headers;
2. main content extracted with [Readability](https://codeberg.org/readeck/go-readability) (goquery as a fallback);
3. repeated lines (menus, cookie banners) dropped and whitespace squeezed;
4. if the site blocks the request (401/403/429/503, challenge page, JavaScript-only page) and `FETCH_FALLBACK=jina`, one retry through r.jina.ai;
5. cut to `SUMMARY_MAX_CHARS`, keeping the first 70% and the last 30%.

## Quick start
```bash
mkdir megabot && cd megabot
curl -O https://raw.githubusercontent.com/DangerBlack/megabot/main/docker-compose.yml
curl -o .env https://raw.githubusercontent.com/DangerBlack/megabot/main/.env.example
# edit .env: TOKEN, ROOT_USER_ID and at least one provider
docker compose up -d
```
Images are published for amd64 and arm64 as `dangerblack/megabot`. Data (users, usage log, conversation messages) lives in `./data/megabot.sqlite`; messages are deleted after `MESSAGE_RETENTION_DAYS`.

The bot uses long polling, so it needs no open ports. A Telegram token can have only one consumer: stop any other bot or webhook that uses the same token first, or the bot gets `409 Conflict`.

## Configuration
Everything is in [`.env.example`](.env.example). Required: `TOKEN`, `ROOT_USER_ID` and at least one provider (`OPENAI_API_KEY` and/or `LLM_PROVIDERS`). Transcription and image generation use `TRANSCRIBE_PROVIDER` / `IMAGE_PROVIDER` (default `openai`); without one, those features are turned off and the rest keeps working.

## Development
```bash
go test ./...          # unit tests + end-to-end tests against fake Telegram and OpenAI servers
go run ./src/main.go
```
Pushing to `main` builds and publishes the Docker image (GitHub Actions).
