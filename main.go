package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
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

const webhookPath = "/api/webhook/tg"

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	envBotToken := os.Getenv("BOT_TOKEN")

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

	botEnabled := new(atomic.Bool)
	if enabled, err := store.GetBotEnabled(); err == nil {
		botEnabled.Store(enabled)
	} else {
		botEnabled.Store(true)
	}

	app := fiber.New(fiber.Config{
		AppName:           "SpamObserver",
		BodyLimit:         1 * 1024 * 1024,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		StreamRequestBody: true,
	})

	updatesChan := make(chan telego.Update, 256)
	monitor := bot.New(broker, store.GetMonitoredIDs, botEnabled.Load)

	var (
		botMu     sync.Mutex
		botCtx    context.Context
		botCancel context.CancelFunc
		bh        *telegohandler.BotHandler
	)

	startBot := func(token string) error {
		botMu.Lock()
		defer botMu.Unlock()

		if botCancel != nil {
			if bh != nil {
				bh.Stop()
			}
			botCancel()
		}

		telegoBot, err := telego.NewBot(token, telego.WithDiscardLogger())
		if err != nil {
			return fmt.Errorf("create bot: %w", err)
		}

		botCtx, botCancel = context.WithCancel(context.Background())

		bh, err = telegohandler.NewBotHandler(telegoBot, updatesChan)
		if err != nil {
			botCancel()
			return fmt.Errorf("create bot handler: %w", err)
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

		if publicURL != "" {
			go func() {
				time.Sleep(2 * time.Second)
				if err := telegoBot.SetWebhook(botCtx, &telego.SetWebhookParams{
					URL:            publicURL + webhookPath,
					SecretToken:    webhookSecret,
					AllowedUpdates: []string{"message", "chat_member", "my_chat_member", "callback_query"},
				}); err != nil {
					broker.Publish(logstream.Error("SYSTEM", "Failed to set webhook: %v", err))
				} else {
					broker.Publish(logstream.Info("SYSTEM", "Webhook registered at %s", publicURL+webhookPath))
				}
			}()
		} else {
			broker.Publish(logstream.Info("SYSTEM", "Bot started (no PUBLIC_URL, webhook not set)"))
		}

		return nil
	}

	app.Add([]string{"POST"}, webhookPath, func(c fiber.Ctx) error {
		secretHeader := c.Get(telego.WebhookSecretTokenHeader)
		if secretHeader != webhookSecret {
			return c.SendStatus(fiber.StatusForbidden)
		}

		var update telego.Update
		if err := json.Unmarshal(c.Body(), &update); err != nil {
			return c.SendStatus(fiber.StatusBadRequest)
		}

		select {
		case updatesChan <- update:
		default:
		}

		return c.SendStatus(fiber.StatusOK)
	})

	h := handler.New(store, broker, jwt, botEnabled, startBot)
	h.Register(app)

	broker.Publish(logstream.Info("SYSTEM", "SpamObserver starting on port %s", port))

	dbToken, _ := store.GetBotToken()
	switch {
	case dbToken != "":
		broker.Publish(logstream.Info("SYSTEM", "Using bot token from database"))
		if err := startBot(dbToken); err != nil {
			broker.Publish(logstream.Error("SYSTEM", "Failed to start bot with DB token: %v", err))
		}
	case envBotToken != "":
		broker.Publish(logstream.Info("SYSTEM", "Using bot token from environment"))
		if err := startBot(envBotToken); err != nil {
			broker.Publish(logstream.Error("SYSTEM", "Failed to start bot with env token: %v", err))
		}
	default:
		broker.Publish(logstream.Warn("SYSTEM", "No bot token configured. Set one via Settings to start monitoring."))
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		broker.Publish(logstream.Info("SYSTEM", "Shutdown signal received"))
		botMu.Lock()
		if botCancel != nil {
			if bh != nil {
				bh.Stop()
			}
			botCancel()
		}
		botMu.Unlock()
		_ = app.Shutdown()
	}()

	fmt.Printf("SpamObserver listening on :%s\n", port)
	if err := app.Listen(":" + port); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
