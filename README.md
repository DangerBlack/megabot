# megabot

Assistente personale su Telegram con modelli compatibili OpenAI: OpenAI, Mimir aziendale o qualsiasi endpoint `/v1`. Sostituisce il workflow n8n "Openai workflow".

| Cosa mandi | Cosa fa |
|---|---|
| 🎙 vocale o file audio | trascrizione; se è lunga, il bottone **📝 Riassumi** |
| 🔗 messaggio con un link | riassunto della pagina; **rispondi al riassunto** per fare domande sull'articolo |
| 📷 foto senza didascalia | trascrive il testo nella foto (OCR) |
| 📷 foto con didascalia | la didascalia è la domanda sulla foto ("quanto costa in totale?", "traduci") |
| `/image <prompt>` / `/image-raw <prompt>` | genera un'immagine, come foto o come file PNG |
| `/gpt <domanda>` (in privato basta scrivere) | risposta testuale |
| ↩️ risposta a un messaggio del bot | continua la conversazione (anche nei gruppi) |
| `/llm [nome]` | mostra o cambia il provider di testo e foto |
| `/usage` | richieste e spesa del mese (l'amministratore vede tutti) |

## Accesso
- `ROOT_USER_ID` è l'amministratore: ha accesso completo e nessun budget.
- Chiunque altro scrive **`/login`**. All'amministratore arriva la richiesta con i bottoni **✅ Accetta** (budget `DEFAULT_USER_BUDGET_USD`/mese), **♾ Senza limite** e **❌ Rifiuta**.
- Comandi dell'amministratore: `/users`, `/revoke <id>`, `/budget <id> <usd|none>`.
- Chi non è autorizzato riceve solo l'indicazione di usare `/login`. I rifiutati vengono ignorati, e nei gruppi il bot tace.

## Costi e budget
Ogni richiesta registra provider, modello, token, secondi di audio e immagini generate, più il **costo stimato** calcolato dai prezzi in `.env`. I prezzi OpenAI di default sono stime da verificare. Quando un utente supera il budget mensile, le funzioni a pagamento si fermano. I provider con prezzo 0, come Mimir, restano utilizzabili, e il bot suggerisce `/llm`.

## Riassunto delle pagine
1. GET con header da browser;
2. estrazione del contenuto principale con [Readability](https://codeberg.org/readeck/go-readability), con goquery come riserva;
3. eliminazione delle righe ripetute (menu, banner dei cookie) e compattazione degli spazi;
4. se il sito blocca (401/403/429/503, pagina di verifica, pagina solo JavaScript) e `FETCH_FALLBACK=jina`, nuovo tentativo tramite r.jina.ai;
5. taglio a `SUMMARY_MAX_CHARS`, tenendo il 70% iniziale e il 30% finale.

## Configurazione
Vedi `.env.example`. Sono obbligatori `TOKEN`, `ROOT_USER_ID` e almeno un provider: `OPENAI_API_KEY` e/o `LLM_PROVIDERS`. Trascrizione e immagini usano `TRANSCRIBE_PROVIDER` / `IMAGE_PROVIDER` (per default `openai`); senza OpenAI quelle funzioni vengono disattivate.

## Sviluppo
```bash
go test ./...          # unit test + test end-to-end con finti Telegram e OpenAI
go run ./src/main.go
```

## Deploy
Il push su `main` costruisce `dangerblack/megabot` per amd64 e arm64 (GitHub Actions). Per avviarlo: `docker compose up -d`.
Senza registry: `cp deploy.env.example deploy.env`, poi `./deploy.sh copia` (costruisce l'immagine e la copia via ssh), `./deploy.sh avvia`, `./deploy.sh log` e `./deploy.sh rollback`.
Il database `data/megabot.sqlite` contiene utenti, log d'uso e i messaggi delle conversazioni. Questi ultimi vengono cancellati dopo `MESSAGE_RETENTION_DAYS`.

## Migrazione da n8n
Un token Telegram ammette **un solo** consumatore di aggiornamenti. Finché il workflow n8n è attivo, il long polling riceve `409 Conflict`. Per passare a megabot:
1. disattiva "Openai workflow" su n8n, che rimuove il webhook;
2. avvia megabot con lo **stesso** `TOKEN`.

Per tornare indietro: ferma megabot e riattiva il workflow.
