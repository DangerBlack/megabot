package main

import (
	"context"
	"log"
	"megabot/src/config"
	"megabot/src/database"
	"megabot/src/telegram"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"gopkg.in/telebot.v3"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Default().Printf("warn loading .env file: %v", err)
	}

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(cfg.DBPath, 0755); err != nil {
		log.Fatalf("failed to create db path: %v", err)
	}

	db := database.NewDatabase(cfg.DBPath)
	defer db.Close()
	db.CreateTables()
	if err := db.EnsureApproved(cfg.RootUserID, "", ""); err != nil {
		log.Fatal(err)
	}

	bot, err := telebot.NewBot(telebot.Settings{
		Token: cfg.Token,
		Poller: &telebot.LongPoller{
			Timeout:        10 * time.Second,
			AllowedUpdates: []string{"message", "callback_query"},
		},
		OnError: func(err error, c telebot.Context) {
			log.Printf("telebot error: %v", err)
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	tg := telegram.New(bot, db, cfg)
	tg.SetupHandlers()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tg.PruneLoop(ctx)
	if cfg.HealthcheckURL != "" {
		go healthcheck(ctx, cfg.HealthcheckURL)
	}

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Default().Printf("bot @%s started: llm=%v default=%s transcribe=%q image=%q fallback=%s",
			bot.Me.Username, cfg.ProviderOrder, cfg.DefaultLLM, cfg.TranscribeProvider, cfg.ImageProvider, cfg.FetchFallback)
		bot.Start()
	}()

	<-signalChan
	log.Default().Println("shutdown signal received.")
	bot.Stop()
	log.Default().Println("shutdown complete.")
}

// healthcheck pings an external monitor (e.g. healthchecks.io) once a minute.
func healthcheck(ctx context.Context, url string) {
	client := &http.Client{Timeout: 10 * time.Second}
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if resp, err := client.Do(req); err != nil {
			log.Printf("healthcheck failed: %v", err)
		} else {
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
