package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
)

const (
	CookieName   = "spamo_session"
	TokenExpiry  = 24 * time.Hour
	RefreshAfter = 12 * time.Hour
)

type Claims struct {
	User string
	Exp  time.Time
}

type JWTManager struct {
	secret []byte
	mu     sync.RWMutex
}

func NewJWTManager() *JWTManager {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		b := make([]byte, 32)
		rand.Read(b)
		secret = hex.EncodeToString(b)
	}
	return &JWTManager{secret: []byte(secret)}
}

func (j *JWTManager) Generate(username string) (string, error) {
	now := time.Now()
	exp := now.Add(TokenExpiry)
	payload := fmt.Sprintf("%s|%d|%d", username, now.Unix(), exp.Unix())
	mac := hmac.New(sha256.New, j.secret)
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s.%s", payload, sig), nil
}

func (j *JWTManager) Validate(token string) (*Claims, error) {
	idx := -1
	for i := len(token) - 1; i >= 0; i-- {
		if token[i] == '.' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("invalid token format")
	}

	payload := token[:idx]
	sigHex := token[idx+1:]

	mac := hmac.New(sha256.New, j.secret)
	mac.Write([]byte(payload))
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sigHex), []byte(expectedSig)) {
		return nil, fmt.Errorf("invalid signature")
	}

	var username string
	var issuedAt, expiresAt int64
	_, err := fmt.Sscanf(payload, "%s|%d|%d", &username, &issuedAt, &expiresAt)
	if err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}

	exp := time.Unix(expiresAt, 0)
	if time.Now().After(exp) {
		return nil, fmt.Errorf("token expired")
	}

	return &Claims{User: username, Exp: exp}, nil
}

func (j *JWTManager) SetCookie(c fiber.Ctx, token string) {
	c.Cookie(&fiber.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HTTPOnly: true,
		SameSite: "Lax",
		Expires:  time.Now().Add(TokenExpiry),
	})
}

func (j *JWTManager) ClearCookie(c fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HTTPOnly: true,
		MaxAge:   -1,
	})
}

func (j *JWTManager) GetTokenFromCookie(c fiber.Ctx) string {
	return c.Cookies(CookieName)
}

func AuthMiddleware(jwt *JWTManager, verifier func(password string) (bool, error)) fiber.Handler {
	return func(c fiber.Ctx) error {
		token := jwt.GetTokenFromCookie(c)
		if token == "" {
			authHeader := c.Get("Authorization")
			if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
				token = authHeader[7:]
			}
		}

		if token == "" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error": "authentication required",
			})
		}

		claims, err := jwt.Validate(token)
		if err != nil {
			jwt.ClearCookie(c)
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error": "invalid or expired token",
			})
		}

		c.Locals("user", claims.User)
		return c.Next()
	}
}
