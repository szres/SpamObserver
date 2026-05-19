package handler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/spam-observer/internal/ai"
	"github.com/spam-observer/internal/auth"
	"github.com/spam-observer/internal/db"
	"github.com/spam-observer/internal/logstream"
	"github.com/spam-observer/internal/tracker"
	"github.com/spam-observer/internal/webui"
)

type Handler struct {
	store      *db.Store
	broker     *logstream.Broker
	jwt        *auth.JWTManager
	botEnabled *atomic.Bool
	restartBot func(token string) error
	tracker    *tracker.Tracker
}

func New(store *db.Store, broker *logstream.Broker, jwt *auth.JWTManager, botEnabled *atomic.Bool, restartBot func(token string) error, t *tracker.Tracker) *Handler {
	return &Handler{
		store:      store,
		broker:     broker,
		jwt:        jwt,
		botEnabled: botEnabled,
		restartBot: restartBot,
		tracker:    t,
	}
}

func (h *Handler) Register(app *fiber.App) {
	app.Get("/", func(c fiber.Ctx) error {
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.Send(webui.HTML)
	})

	api := app.Group("/api")

	api.Post("/auth/login", h.handleLogin)
	api.Post("/auth/logout", h.handleLogout)
	api.Get("/auth/me", h.handleMe)

	api.Get("/logs/stream", h.handleSSEStream)
	api.Get("/logs/recent", h.handleRecentLogs)

	api.Get("/bot/status", h.handleBotStatus)
	api.Get("/bot/info", h.handleBotInfo)
	api.Get("/bot/new-users-count", h.handleNewUsersCount)

	configGroup := api.Group("/config", auth.AuthMiddleware(h.jwt, nil))
	configGroup.Get("/groups", h.handleListGroups)
	configGroup.Post("/groups", h.handleAddGroup)
	configGroup.Delete("/groups/:chatId", h.handleRemoveGroup)
	configGroup.Post("/bot/toggle", h.handleBotToggle)
	configGroup.Get("/bot/token", h.handleGetBotToken)
	configGroup.Post("/bot/token", h.handleSetBotToken)
	configGroup.Post("/auth/change-password", h.handleChangePassword)
	configGroup.Post("/auth/change-username", h.handleChangeUsername)
	configGroup.Get("/verify-bots", h.handleListVerifyBots)
	configGroup.Post("/verify-bots", h.handleAddVerifyBot)
	configGroup.Delete("/verify-bots/:botId", h.handleRemoveVerifyBot)
	configGroup.Get("/new-users", h.handleListNewUsers)
	configGroup.Get("/ai", h.handleGetAIConfig)
	configGroup.Post("/ai", h.handleSetAIConfig)
	configGroup.Post("/ai/test", h.handleTestAIConfig)
}

func (h *Handler) handleLogin(c fiber.Ctx) error {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.Bind().JSON(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request"})
	}

	admin, err := h.store.GetAdmin()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "admin not configured"})
	}

	if body.Username != admin.Username {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid credentials"})
	}

	ok, err := h.store.VerifyAdminPassword(body.Password)
	if err != nil || !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid credentials"})
	}

	token, err := h.jwt.Generate(body.Username)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "token generation failed"})
	}

	h.jwt.SetCookie(c, token)
	return c.JSON(fiber.Map{"username": body.Username, "token": token})
}

func (h *Handler) handleLogout(c fiber.Ctx) error {
	h.jwt.ClearCookie(c)
	return c.JSON(fiber.Map{"ok": true})
}

func (h *Handler) handleMe(c fiber.Ctx) error {
	token := h.jwt.GetTokenFromCookie(c)
	if token == "" {
		authHeader := c.Get("Authorization")
		if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
			token = authHeader[7:]
		}
	}
	if token == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "not authenticated"})
	}
	claims, err := h.jwt.Validate(token)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid token"})
	}
	return c.JSON(fiber.Map{"username": claims.User})
}

func (h *Handler) handleListGroups(c fiber.Ctx) error {
	groups, err := h.store.ListGroups()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list groups"})
	}
	if groups == nil {
		groups = []db.MonitoredGroup{}
	}
	return c.JSON(groups)
}

func (h *Handler) handleAddGroup(c fiber.Ctx) error {
	var body struct {
		ChatID int64 `json:"chat_id"`
	}
	if err := c.Bind().JSON(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request"})
	}
	if body.ChatID == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "chat_id required"})
	}
	if err := h.store.AddGroup(body.ChatID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to add group"})
	}
	h.broker.Publish(logstream.Info("CONFIG", "Group %d added to monitoring", body.ChatID))
	return c.JSON(fiber.Map{"ok": true})
}

func (h *Handler) handleRemoveGroup(c fiber.Ctx) error {
	chatIDStr := c.Params("chatId")
	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid chat_id"})
	}
	if err := h.store.RemoveGroup(chatID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to remove group"})
	}
	h.broker.Publish(logstream.Info("CONFIG", "Group %d removed from monitoring", chatID))
	return c.JSON(fiber.Map{"ok": true})
}

func (h *Handler) handleSSEStream(c fiber.Ctx) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	return c.SendStreamWriter(func(w *bufio.Writer) {
		recent, err := h.store.GetRecentLogs(200)
		if err == nil && len(recent) > 0 {
			data, _ := json.Marshal(recent)
			fmt.Fprintf(w, "event: history\ndata: %s\n\n", data)
			w.Flush()
		}

		ch := h.broker.Subscribe()
		defer h.broker.Unsubscribe(ch)

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case entry, ok := <-ch:
				if !ok {
					return
				}
				data, _ := json.Marshal(entry)
				fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
				w.Flush()
			case <-ticker.C:
				fmt.Fprintf(w, ": keepalive\n\n")
				w.Flush()
			}
		}
	})
}

func (h *Handler) handleRecentLogs(c fiber.Ctx) error {
	recent, err := h.store.GetRecentLogs(200)
	if err != nil {
		recent = []logstream.Entry{}
	}
	return c.JSON(recent)
}

func (h *Handler) handleBotStatus(c fiber.Ctx) error {
	return c.JSON(fiber.Map{"enabled": h.botEnabled.Load()})
}

func (h *Handler) handleBotToggle(c fiber.Ctx) error {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.Bind().JSON(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request"})
	}

	if err := h.store.SetBotEnabled(body.Enabled); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update bot status"})
	}
	h.botEnabled.Store(body.Enabled)

	state := "disabled"
	if body.Enabled {
		state = "enabled"
	}
	h.broker.Publish(logstream.Info("SYSTEM", "Bot monitoring %s", state))

	return c.JSON(fiber.Map{"ok": true, "enabled": body.Enabled})
}

func (h *Handler) handleBotInfo(c fiber.Ctx) error {
	hasToken := h.store.HasBotToken()
	return c.JSON(fiber.Map{
		"has_token": hasToken,
		"enabled":   h.botEnabled.Load(),
	})
}

func (h *Handler) handleNewUsersCount(c fiber.Ctx) error {
	users := h.tracker.GetAllNew()
	return c.JSON(fiber.Map{"count": len(users)})
}

func (h *Handler) handleGetBotToken(c fiber.Ctx) error {
	token, err := h.store.GetBotToken()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to get bot token"})
	}
	masked := ""
	if len(token) > 8 {
		masked = token[:4] + "..." + token[len(token)-4:]
	} else if token != "" {
		masked = "***"
	}
	return c.JSON(fiber.Map{"has_token": token != "", "masked_token": masked})
}

func (h *Handler) handleSetBotToken(c fiber.Ctx) error {
	var body struct {
		Token string `json:"token"`
	}
	if err := c.Bind().JSON(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request"})
	}
	if body.Token == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "token is required"})
	}

	if err := h.store.SetBotToken(body.Token); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save bot token"})
	}

	if h.restartBot != nil {
		if err := h.restartBot(body.Token); err != nil {
			h.broker.Publish(logstream.Error("SYSTEM", "Failed to restart bot: %v", err))
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "token saved but bot restart failed: " + err.Error()})
		}
	}

	h.broker.Publish(logstream.Info("CONFIG", "Bot token updated, bot restarted"))
	return c.JSON(fiber.Map{"ok": true})
}

func (h *Handler) handleChangePassword(c fiber.Ctx) error {
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := c.Bind().JSON(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request"})
	}
	if body.CurrentPassword == "" || body.NewPassword == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "both current and new password are required"})
	}
	if len(body.NewPassword) < 4 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "new password must be at least 4 characters"})
	}

	ok, err := h.store.VerifyAdminPassword(body.CurrentPassword)
	if err != nil || !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "current password is incorrect"})
	}

	if err := h.store.UpdateAdminPassword(body.NewPassword); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update password"})
	}

	h.broker.Publish(logstream.Info("CONFIG", "Admin password changed"))
	return c.JSON(fiber.Map{"ok": true})
}

func (h *Handler) handleChangeUsername(c fiber.Ctx) error {
	var body struct {
		NewUsername string `json:"new_username"`
	}
	if err := c.Bind().JSON(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request"})
	}
	if body.NewUsername == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "new username is required"})
	}

	if err := h.store.UpdateAdminUsername(body.NewUsername); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update username"})
	}

	h.broker.Publish(logstream.Info("CONFIG", "Admin username changed to %s", body.NewUsername))
	return c.JSON(fiber.Map{"ok": true, "username": body.NewUsername})
}

func (h *Handler) handleListVerifyBots(c fiber.Ctx) error {
	bots, err := h.store.ListVerificationBots()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list verification bots"})
	}
	if bots == nil {
		bots = []db.VerificationBot{}
	}
	return c.JSON(bots)
}

func (h *Handler) handleAddVerifyBot(c fiber.Ctx) error {
	var body struct {
		BotID int64  `json:"bot_id"`
		Label string `json:"label"`
	}
	if err := c.Bind().JSON(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request"})
	}
	if body.BotID == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bot_id required"})
	}
	if err := h.store.AddVerificationBot(body.BotID, body.Label); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to add verification bot"})
	}
	h.broker.Publish(logstream.Info("CONFIG", "Verification bot %d (%s) added", body.BotID, body.Label))
	return c.JSON(fiber.Map{"ok": true})
}

func (h *Handler) handleRemoveVerifyBot(c fiber.Ctx) error {
	botIDStr := c.Params("botId")
	botID, err := strconv.ParseInt(botIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid bot_id"})
	}
	if err := h.store.RemoveVerificationBot(botID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to remove verification bot"})
	}
	h.broker.Publish(logstream.Info("CONFIG", "Verification bot %d removed", botID))
	return c.JSON(fiber.Map{"ok": true})
}

func (h *Handler) handleListNewUsers(c fiber.Ctx) error {
	users := h.tracker.GetAllNew()
	if users == nil {
		users = []tracker.UserInfo{}
	}
	return c.JSON(users)
}

func (h *Handler) handleGetAIConfig(c fiber.Ctx) error {
	cfg := h.store.GetAIConfigMasked()
	return c.JSON(cfg)
}

func (h *Handler) handleSetAIConfig(c fiber.Ctx) error {
	var body db.AIConfig
	if err := c.Bind().JSON(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request"})
	}
	if err := h.store.SetAIConfig(body); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save AI config"})
	}
	h.broker.Publish(logstream.Info("CONFIG", "AI config updated (model: %s)", body.Model))
	return c.JSON(fiber.Map{"ok": true})
}

func (h *Handler) handleTestAIConfig(c fiber.Ctx) error {
	cfg, err := h.store.GetAIConfig()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to get AI config"})
	}
	if cfg.BaseURL == "" || cfg.APIKey == "" || cfg.Model == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "AI config incomplete"})
	}

	resolvedURL := ai.BuildAPIURL(cfg.BaseURL)
	ctx := c.Context()
	aiCfg := ai.Config{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Model: cfg.Model}
	testErr := ai.TestConnection(ctx, aiCfg)
	if testErr != nil {
		return c.JSON(fiber.Map{"ok": false, "error": testErr.Error(), "url": resolvedURL})
	}
	return c.JSON(fiber.Map{"ok": true, "url": resolvedURL})
}

func InitAdmin(store *db.Store) error {
	_, err := store.GetAdmin()
	if err == db.ErrNotFound {
		password := os.Getenv("INITIAL_ADMIN_PASSWORD")
		if password == "" {
			password = "admin"
		}
		username := os.Getenv("INITIAL_ADMIN_USERNAME")
		if username == "" {
			username = "admin"
		}
		return store.InitAdmin(username, password)
	}
	return err
}
