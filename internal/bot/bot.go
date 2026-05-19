package bot

import (
	"fmt"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	"github.com/spam-observer/internal/logstream"
)

type Monitor struct {
	broker    *logstream.Broker
	monitored func() map[int64]struct{}
}

func New(broker *logstream.Broker, monitored func() map[int64]struct{}) *Monitor {
	return &Monitor{
		broker:    broker,
		monitored: monitored,
	}
}

func (m *Monitor) ProcessUpdate(update telego.Update) {
	if update.Message != nil {
		m.processMessage(update.Message)
	}
	if update.ChatMember != nil {
		m.processChatMemberUpdate(update.ChatMember)
	}
	if update.MyChatMember != nil {
		m.processChatMemberUpdate(update.MyChatMember)
	}
	if update.CallbackQuery != nil {
		m.processCallbackQuery(update.CallbackQuery)
	}
}

func (m *Monitor) isMonitored(chatID int64) bool {
	ids := m.monitored()
	if len(ids) == 0 {
		return false
	}
	_, ok := ids[chatID]
	return ok
}

func (m *Monitor) processMessage(msg *telego.Message) {
	chatID := msg.Chat.ID
	if !m.isMonitored(chatID) {
		return
	}

	if len(msg.NewChatMembers) > 0 {
		for _, member := range msg.NewChatMembers {
			entry := logstream.Entry{
				Timestamp: time.Unix(int64(msg.Date), 0),
				Level:     "INFO",
				Category:  "JOIN",
				ChatID:    chatID,
				UserID:    member.ID,
				Username:  member.Username,
				Message: fmt.Sprintf("New member joined: %s (ID: %d, Bot: %v)",
					memberDisplayName(member), member.ID, member.IsBot),
			}
			if member.IsBot {
				entry.Level = "WARN"
				entry.Category = "BOT_JOIN"
				entry.Message = fmt.Sprintf("Bot added to group: @%s (ID: %d)", member.Username, member.ID)
			}
			m.broker.Publish(entry)
		}
	}

	if msg.LeftChatMember != nil {
		member := msg.LeftChatMember
		m.broker.Publish(logstream.Entry{
			Timestamp: time.Unix(int64(msg.Date), 0),
			Level:     "INFO",
			Category:  "LEAVE",
			ChatID:    chatID,
			UserID:    member.ID,
			Username:  member.Username,
			Message:   fmt.Sprintf("Member left: %s (ID: %d)", memberDisplayName(*member), member.ID),
		})
	}

	if msg.From != nil && msg.From.IsBot {
		m.broker.Publish(logstream.Entry{
			Timestamp: time.Unix(int64(msg.Date), 0),
			Level:     "INFO",
			Category:  "BOT_MSG",
			ChatID:    chatID,
			UserID:    msg.From.ID,
			Username:  msg.From.Username,
			Message:   fmt.Sprintf("Bot message from @%s: %s", msg.From.Username, truncate(extractText(msg), 200)),
			Raw:       extractText(msg),
		})
	}

	if msg.Text != "" || msg.Caption != "" {
		m.analyzeContent(msg)
	}

	if msg.Entities != nil || msg.CaptionEntities != nil {
		m.analyzeEntities(msg)
	}
}

func (m *Monitor) processChatMemberUpdate(update *telego.ChatMemberUpdated) {
	chatID := update.Chat.ID
	if !m.isMonitored(chatID) {
		return
	}

	newMember := update.NewChatMember
	status := newMember.MemberStatus()
	var level, category, action string

	switch status {
	case telego.MemberStatusRestricted:
		level = "WARN"
		category = "RESTRICT"
		if r, ok := newMember.(*telego.ChatMemberRestricted); ok {
			if !r.CanSendMessages {
				action = "muted"
			} else {
				action = "restricted"
			}
		} else {
			action = "restricted"
		}
	case telego.MemberStatusBanned:
		level = "WARN"
		category = "BAN"
		action = "banned"
	case telego.MemberStatusLeft:
		level = "INFO"
		category = "REMOVE"
		action = "removed"
	case telego.MemberStatusMember:
		level = "INFO"
		category = "PROMOTE"
		action = "promoted to member"
	case telego.MemberStatusAdministrator:
		level = "INFO"
		category = "ADMIN"
		action = "promoted to admin"
	case telego.MemberStatusCreator:
		level = "INFO"
		category = "ADMIN"
		action = "is now creator"
	default:
		return
	}

	performedBy := "unknown"
	if update.From.ID != 0 {
		performedBy = fmt.Sprintf("%s (ID: %d)", memberDisplayName(update.From), update.From.ID)
	}

	targetUser := newMember.MemberUser()

	m.broker.Publish(logstream.Entry{
		Timestamp: time.Unix(int64(update.Date), 0),
		Level:     level,
		Category:  category,
		ChatID:    chatID,
		UserID:    targetUser.ID,
		Username:  targetUser.Username,
		Message: fmt.Sprintf("%s was %s by %s",
			memberDisplayName(targetUser), action, performedBy),
	})
}

func (m *Monitor) processCallbackQuery(cq *telego.CallbackQuery) {
	if cq.Message == nil {
		return
	}
	chat := cq.Message.GetChat()
	chatID := chat.ID
	if !m.isMonitored(chatID) {
		return
	}

	from := cq.From

	m.broker.Publish(logstream.Entry{
		Timestamp: time.Now(),
		Level:     "INFO",
		Category:  "BUTTON",
		ChatID:    chatID,
		UserID:    from.ID,
		Username:  from.Username,
		Message: fmt.Sprintf("Button click by %s: %q",
			memberDisplayName(from), cq.Data),
		Raw: cq.Data,
	})
}

func (m *Monitor) analyzeContent(msg *telego.Message) {
	text := extractText(msg)
	if text == "" {
		return
	}

	lower := strings.ToLower(text)

	adIndicators := []struct {
		pattern  string
		category string
	}{
		{"t.me/", "AD_LINK"},
		{"joinchat", "AD_LINK"},
		{"addurl", "AD_LINK"},
		{"crypto", "AD_KEYWORD"},
		{"invest", "AD_KEYWORD"},
		{"earn money", "AD_KEYWORD"},
		{"free gift", "AD_KEYWORD"},
		{"click here", "AD_KEYWORD"},
		{"dm me", "AD_KEYWORD"},
		{"whatsapp", "AD_KEYWORD"},
		{"signal group", "AD_KEYWORD"},
		{"promo", "AD_KEYWORD"},
		{"discount", "AD_KEYWORD"},
		{"@admin", "MENTION"},
		{"/verify", "COMMAND"},
		{"/captcha", "COMMAND"},
		{"/ban", "COMMAND"},
		{"/kick", "COMMAND"},
		{"/mute", "COMMAND"},
		{"/restrict", "COMMAND"},
		{"/report", "COMMAND"},
	}

	for _, indicator := range adIndicators {
		if strings.Contains(lower, indicator.pattern) {
			userID := int64(0)
			username := ""
			if msg.From != nil {
				userID = msg.From.ID
				username = msg.From.Username
			}

			level := "INFO"
			if indicator.category == "AD_LINK" || indicator.category == "AD_KEYWORD" {
				level = "WARN"
			}

			m.broker.Publish(logstream.Entry{
				Timestamp: time.Unix(int64(msg.Date), 0),
				Level:     level,
				Category:  indicator.category,
				ChatID:    msg.Chat.ID,
				UserID:    userID,
				Username:  username,
				Message: fmt.Sprintf("Detected [%s] in message: %s",
					indicator.category, truncate(text, 200)),
				Raw: text,
			})
		}
	}
}

func (m *Monitor) analyzeEntities(msg *telego.Message) {
	entities := msg.Entities
	if len(entities) == 0 {
		entities = msg.CaptionEntities
	}
	if len(entities) == 0 {
		return
	}

	for _, entity := range entities {
		var category string
		switch entity.Type {
		case telego.EntityTypeURL:
			category = "URL_ENTITY"
		case telego.EntityTypeTextLink:
			category = "TEXT_LINK"
		case telego.EntityTypeMention:
			category = "MENTION"
		case telego.EntityTypeHashtag:
			category = "HASHTAG"
		case telego.EntityTypeBotCommand:
			category = "BOT_COMMAND"
		default:
			continue
		}

		userID := int64(0)
		username := ""
		if msg.From != nil {
			userID = msg.From.ID
			username = msg.From.Username
		}

		text := msg.Text
		if text == "" {
			text = msg.Caption
		}
		extracted := extractEntityText(text, entity)

		extra := ""
		if entity.Type == telego.EntityTypeTextLink && entity.URL != "" {
			extra = fmt.Sprintf(" -> %s", entity.URL)
		}

		m.broker.Publish(logstream.Entry{
			Timestamp: time.Unix(int64(msg.Date), 0),
			Level:     "INFO",
			Category:  category,
			ChatID:    msg.Chat.ID,
			UserID:    userID,
			Username:  username,
			Message: fmt.Sprintf("Entity [%s]: %s%s",
				category, truncate(extracted, 100), extra),
			Raw: extracted,
		})
	}
}

func extractText(msg *telego.Message) string {
	if msg.Text != "" {
		return msg.Text
	}
	if msg.Caption != "" {
		return msg.Caption
	}
	return ""
}

func extractEntityText(text string, entity telego.MessageEntity) string {
	runes := []rune(text)
	offset := entity.Offset
	length := entity.Length
	if offset >= len(runes) {
		return ""
	}
	end := offset + length
	if end > len(runes) {
		end = len(runes)
	}
	return string(runes[offset:end])
}

func memberDisplayName(u telego.User) string {
	if u.FirstName != "" && u.LastName != "" {
		return u.FirstName + " " + u.LastName
	}
	if u.FirstName != "" {
		return u.FirstName
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	return fmt.Sprintf("User#%d", u.ID)
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
