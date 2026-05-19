package handler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/spam-observer/internal/auth"
	"github.com/spam-observer/internal/db"
	"github.com/spam-observer/internal/logstream"
	"github.com/spam-observer/internal/webui"
)

type Handler struct {
	store  *db.Store
	broker *logstream.Broker
	jwt    *auth.JWTManager
}

func New(store *db.Store, broker *logstream.Broker, jwt *auth.JWTManager) *Handler {
	return &Handler{
		store:  store,
		broker: broker,
		jwt:    jwt,
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

	configGroup := api.Group("/config", auth.AuthMiddleware(h.jwt, nil))
	configGroup.Get("/groups", h.handleListGroups)
	configGroup.Post("/groups", h.handleAddGroup)
	configGroup.Delete("/groups/:chatId", h.handleRemoveGroup)
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
		recent := h.broker.Recent()
		if len(recent) > 0 {
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
	recent := h.broker.Recent()
	if recent == nil {
		recent = []logstream.Entry{}
	}
	return c.JSON(recent)
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
