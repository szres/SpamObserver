package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegohandler"
	"github.com/spam-observer/internal/auth"
	"github.com/spam-observer/internal/bot"
	"github.com/spam-observer/internal/db"
	"github.com/spam-observer/internal/handler"
	"github.com/spam-observer/internal/logstream"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	botToken := os.Getenv("BOT_TOKEN")
	if botToken == "" {
		log.Fatal("BOT_TOKEN environment variable is required")
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./data/spam-observer.db"
	}

	webhookSecret := os.Getenv("WEBHOOK_SECRET")
	if webhookSecret == "" {
		webhookSecret = "spamo-whsec"
	}

	publicURL := os.Getenv("PUBLIC_URL")

	if err := os.MkdirAll("./data", 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}

	store, err := db.New(dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer store.Close()

	if err := handler.InitAdmin(store); err != nil {
		log.Fatalf("Failed to initialize admin: %v", err)
	}

	broker := logstream.NewBroker()
	defer broker.Close()

	jwt := auth.NewJWTManager()

	h := handler.New(store, broker, jwt)

	app := fiber.New(fiber.Config{
		AppName:           "SpamObserver",
		BodyLimit:         1 * 1024 * 1024,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		StreamRequestBody: true,
	})

	h.Register(app)

	telegoBot, err := telego.NewBot(botToken, telego.WithDiscardLogger())
	if err != nil {
		log.Fatalf("Failed to create telego bot: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	webhookPath := "/api/webhook/tg/" + botToken

	updatesChan, err := telegoBot.UpdatesViaWebhook(ctx,
		func(handler telego.WebhookHandler) error {
			app.Add([]string{"POST"}, webhookPath, func(c fiber.Ctx) error {
				secretHeader := c.Get(telego.WebhookSecretTokenHeader)
				if secretHeader != webhookSecret {
					return c.SendStatus(fiber.StatusForbidden)
				}
				reqCtx := context.WithoutCancel(c.Context())
				if err := handler(reqCtx, c.Body()); err != nil {
					return c.SendStatus(fiber.StatusInternalServerError)
				}
				return c.SendStatus(fiber.StatusOK)
			})
			return nil
		},
		telego.WithWebhookBuffer(256),
	)
	if err != nil {
		log.Fatalf("Failed to setup webhook: %v", err)
	}

	monitor := bot.New(broker, store.GetMonitoredIDs)

	bh, err := telegohandler.NewBotHandler(telegoBot, updatesChan)
	if err != nil {
		log.Fatalf("Failed to create bot handler: %v", err)
	}

	bh.Handle(func(ctx *telegohandler.Context, update telego.Update) error {
		monitor.ProcessUpdate(update)
		return nil
	}, telegohandler.Any())

	go func() {
		if err := bh.Start(); err != nil {
			broker.Publish(logstream.Error("SYSTEM", "Bot handler stopped: %v", err))
		}
	}()

	broker.Publish(logstream.Info("SYSTEM", "SpamObserver starting on port %s", port))
	broker.Publish(logstream.Info("SYSTEM", "Webhook path: %s", webhookPath))

	if publicURL != "" {
		go func() {
			time.Sleep(2 * time.Second)
			if err := telegoBot.SetWebhook(ctx, &telego.SetWebhookParams{
				URL:            publicURL + webhookPath,
				SecretToken:    webhookSecret,
				AllowedUpdates: []string{"message", "chat_member", "my_chat_member", "callback_query"},
			}); err != nil {
				broker.Publish(logstream.Error("SYSTEM", "Failed to set webhook: %v", err))
			} else {
				broker.Publish(logstream.Info("SYSTEM", "Webhook registered at %s%s", publicURL, webhookPath))
			}
		}()
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		broker.Publish(logstream.Info("SYSTEM", "Shutdown signal received"))
		cancel()
		bh.Stop()
		_ = app.Shutdown()
	}()

	fmt.Printf("SpamObserver listening on :%s\n", port)
	if err := app.Listen(":" + port); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
