package bot

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mymmrac/telego"
	"github.com/spam-observer/internal/logstream"
	"github.com/spam-observer/internal/tracker"
)

type Monitor struct {
	broker     *logstream.Broker
	monitored  func() map[int64]struct{}
	enabled    func() bool
	tracker    *tracker.Tracker
	bot        atomic.Pointer[telego.Bot]
	verifyBots func() map[int64]struct{}
}

func New(
	broker *logstream.Broker,
	monitored func() map[int64]struct{},
	enabled func() bool,
	t *tracker.Tracker,
	verifyBots func() map[int64]struct{},
) *Monitor {
	return &Monitor{
		broker:     broker,
		monitored:  monitored,
		enabled:    enabled,
		tracker:    t,
		verifyBots: verifyBots,
	}
}

func (m *Monitor) SetBot(b *telego.Bot) {
	m.bot.Store(b)
}

func (m *Monitor) ProcessUpdate(update telego.Update) {
	if m.enabled != nil && !m.enabled() {
		return
	}
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

func (m *Monitor) isVerifyBot(userID int64) bool {
	ids := m.verifyBots()
	if len(ids) == 0 {
		return false
	}
	_, ok := ids[userID]
	return ok
}

func (m *Monitor) isNewUser(userID int64) bool {
	return m.tracker.IsNew(userID)
}

func (m *Monitor) processMessage(msg *telego.Message) {
	chatID := msg.Chat.ID
	if !m.isMonitored(chatID) {
		return
	}

	if len(msg.NewChatMembers) > 0 {
		for _, member := range msg.NewChatMembers {
			bio := ""
			if !member.IsBot {
				bio = m.fetchUserBio(member.ID)
			}
			displayName := memberDisplayName(member)

			if !member.IsBot {
				m.tracker.MarkNew(member.ID, chatID, displayName, member.Username, bio)
			}

			bioDisplay := bio
			if bioDisplay == "" {
				bioDisplay = "(none)"
			}

			entry := logstream.Entry{
				Timestamp: time.Unix(int64(msg.Date), 0),
				Level:     "INFO",
				Category:  "JOIN",
				ChatID:    chatID,
				UserID:    member.ID,
				Username:  member.Username,
				IsNew:     !member.IsBot,
				Message: fmt.Sprintf("New member joined: %s (ID: %d, Username: @%s, Bio: %s, Bot: %v)",
					displayName, member.ID, member.Username, bioDisplay, member.IsBot),
			}
			if member.IsBot {
				entry.Level = "WARN"
				entry.Category = "BOT_JOIN"
				entry.IsNew = false
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
		isVerify := m.isVerifyBot(msg.From.ID)
		category := "BOT_MSG"
		if isVerify {
			category = "VERIFY_BOT_MSG"
		}
		m.broker.Publish(logstream.Entry{
			Timestamp: time.Unix(int64(msg.Date), 0),
			Level:     "INFO",
			Category:  category,
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

	m.logQuote(msg, chatID)
}

func (m *Monitor) fetchUserBio(userID int64) string {
	b := m.bot.Load()
	if b == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := b.GetChat(ctx, &telego.GetChatParams{
		ChatID: telego.ChatID{ID: userID},
	})
	if err != nil {
		return ""
	}
	return info.Bio
}

func (m *Monitor) logQuote(msg *telego.Message, chatID int64) {
	hasQuote := msg.Quote != nil && msg.Quote.Text != ""
	hasReply := msg.ReplyToMessage != nil

	if !hasQuote && !hasReply {
		return
	}

	userID := int64(0)
	username := ""
	if msg.From != nil {
		userID = msg.From.ID
		username = msg.From.Username
	}

	parts := []string{}
	rawParts := []string{}

	if hasQuote {
		quoteText := msg.Quote.Text
		parts = append(parts, fmt.Sprintf("[Quote: %s]", truncate(quoteText, 200)))
		rawParts = append(rawParts, "QUOTE: "+quoteText)
	}

	if hasReply {
		reply := msg.ReplyToMessage
		replyText := extractText(reply)
		replyFrom := "unknown"
		if reply.From != nil {
			replyFrom = fmt.Sprintf("%s (ID: %d)", memberDisplayName(*reply.From), reply.From.ID)
		}
		if replyText != "" {
			parts = append(parts, fmt.Sprintf("[Reply to %s: %s]", replyFrom, truncate(replyText, 200)))
			rawParts = append(rawParts, fmt.Sprintf("REPLY(%s): %s", replyFrom, replyText))
		} else if reply.Photo != nil {
			parts = append(parts, fmt.Sprintf("[Reply to %s: <photo>]", replyFrom))
			rawParts = append(rawParts, fmt.Sprintf("REPLY(%s): <photo>", replyFrom))
		} else if reply.Document != nil {
			parts = append(parts, fmt.Sprintf("[Reply to %s: <document>]", replyFrom))
			rawParts = append(rawParts, fmt.Sprintf("REPLY(%s): <document>", replyFrom))
		} else if reply.Video != nil {
			parts = append(parts, fmt.Sprintf("[Reply to %s: <video>]", replyFrom))
			rawParts = append(rawParts, fmt.Sprintf("REPLY(%s): <video>", replyFrom))
		} else if reply.Sticker != nil {
			parts = append(parts, fmt.Sprintf("[Reply to %s: <sticker>]", replyFrom))
			rawParts = append(rawParts, fmt.Sprintf("REPLY(%s): <sticker>", replyFrom))
		} else {
			parts = append(parts, fmt.Sprintf("[Reply to %s: <non-text>]", replyFrom))
			rawParts = append(rawParts, fmt.Sprintf("REPLY(%s): <non-text>", replyFrom))
		}

		if reply.Entities != nil {
			for _, e := range reply.Entities {
				if e.Type == telego.EntityTypeTextLink && e.URL != "" {
					rawParts = append(rawParts, fmt.Sprintf("REPLY_LINK: %s", e.URL))
				}
			}
		}
	}

	currentText := extractText(msg)
	if currentText != "" {
		parts = append([]string{truncate(currentText, 100)}, parts...)
	}

	m.broker.Publish(logstream.Entry{
		Timestamp: time.Unix(int64(msg.Date), 0),
		Level:     "INFO",
		Category:  "QUOTE",
		ChatID:    chatID,
		UserID:    userID,
		Username:  username,
		IsNew:     m.isNewUser(userID),
		Message:   strings.Join(parts, " "),
		Raw:       strings.Join(rawParts, "\n"),
	})
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
	actorIsVerifyBot := false
	if update.From.ID != 0 {
		performedBy = fmt.Sprintf("%s (ID: %d)", memberDisplayName(update.From), update.From.ID)
		actorIsVerifyBot = m.isVerifyBot(update.From.ID)
	}

	targetUser := newMember.MemberUser()
	targetIsNew := m.isNewUser(targetUser.ID)

	if actorIsVerifyBot && targetIsNew {
		switch status {
		case telego.MemberStatusBanned:
			category = "VERIFY_BAN"
			level = "WARN"
		case telego.MemberStatusRestricted:
			category = "VERIFY_RESTRICT"
			level = "WARN"
		}
	}

	m.broker.Publish(logstream.Entry{
		Timestamp: time.Unix(int64(update.Date), 0),
		Level:     level,
		Category:  category,
		ChatID:    chatID,
		UserID:    targetUser.ID,
		Username:  targetUser.Username,
		IsNew:     targetIsNew,
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
	isNew := m.isNewUser(from.ID)

	category := "BUTTON"
	if isNew {
		if msg := cq.Message.Message(); msg != nil && msg.From.IsBot && m.isVerifyBot(msg.From.ID) {
			category = "VERIFY_BUTTON"
		}
	}

	m.broker.Publish(logstream.Entry{
		Timestamp: time.Now(),
		Level:     "INFO",
		Category:  category,
		ChatID:    chatID,
		UserID:    from.ID,
		Username:  from.Username,
		IsNew:     isNew,
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
				IsNew:     m.isNewUser(userID),
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
			IsNew:     m.isNewUser(userID),
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
