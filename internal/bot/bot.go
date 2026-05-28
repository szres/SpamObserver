package bot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mymmrac/telego"
	"github.com/spam-observer/internal/ai"
	"github.com/spam-observer/internal/logstream"
	"github.com/spam-observer/internal/tracker"
)

type Monitor struct {
	broker      *logstream.Broker
	monitored   func() map[int64]struct{}
	enabled     func() bool
	tracker     *tracker.Tracker
	bot         atomic.Pointer[telego.Bot]
	verifyBots  func() map[int64]struct{}
	aiConfig    func() *ai.Config
	updateTitle func(chatID int64, title string)
	warnInGroup func() bool

	floodJoins    map[int64][]time.Time
	floodActive   map[int64]bool
	floodLastJoin map[int64]time.Time
	floodMu       sync.Mutex
}

func New(
	broker *logstream.Broker,
	monitored func() map[int64]struct{},
	enabled func() bool,
	t *tracker.Tracker,
	verifyBots func() map[int64]struct{},
	aiConfig func() *ai.Config,
	updateTitle func(chatID int64, title string),
	warnInGroup func() bool,
) *Monitor {
	m := &Monitor{
		broker:        broker,
		monitored:     monitored,
		enabled:       enabled,
		tracker:       t,
		verifyBots:    verifyBots,
		aiConfig:      aiConfig,
		updateTitle:   updateTitle,
		warnInGroup:   warnInGroup,
		floodJoins:    make(map[int64][]time.Time),
		floodActive:   make(map[int64]bool),
		floodLastJoin: make(map[int64]time.Time),
	}
	go m.startFloodCleanup()
	return m
}

func (m *Monitor) SetBot(b *telego.Bot) {
	m.bot.Store(b)
}

func (m *Monitor) ProcessUpdate(update telego.Update) {
	if m.enabled != nil && !m.enabled() {
		return
	}
	if update.Message != nil {
		m.processMessage(update.Message, "message")
	}
	if update.BusinessMessage != nil {
		m.processMessage(update.BusinessMessage, "business")
	}
	if update.GuestMessage != nil {
		m.processMessage(update.GuestMessage, "guest")
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

func (m *Monitor) markNewUser(userID, chatID int64, displayName, username, bio string) {
	if bio == "" {
		bio = m.fetchUserBio(userID)
	}

	isNew := m.tracker.TryMarkNew(userID, chatID, displayName, username, bio)
	if !isNew {
		return
	}

	m.recordJoin(chatID)

	bioDisplay := bio
	if bioDisplay == "" {
		bioDisplay = "(none)"
	}
	m.broker.Publish(logstream.Entry{
		Timestamp: time.Now(),
		Level:     "INFO",
		Category:  "NEW_USER",
		ChatID:    chatID,
		UserID:    userID,
		Username:  username,
		IsNew:     true,
		Message: fmt.Sprintf("New user: %s (@%s, Bio: %s)",
			userRef(userID, displayName), username, bioDisplay),
	})

	if m.isFloodActive(chatID) {
		m.broker.Publish(logstream.Entry{
			Timestamp: time.Now(),
			Level:     "WARN",
			Category:  "SPAM_CONFIRMED",
			ChatID:    chatID,
			UserID:    userID,
			Username:  username,
			IsNew:     true,
			Tags:      []string{"FLOOD"},
			Message: fmt.Sprintf("[FLOOD] Direct SPAM: %s (@%s) — flood mode, AI skipped",
				userRef(userID, displayName), username),
		})
	} else {
		go m.assessUserSpam(userID, chatID, displayName, username, bio)
	}
}

func (m *Monitor) assessUserSpam(userID, chatID int64, displayName, username, bio string) {
	if m.aiConfig == nil {
		return
	}
	cfg := m.aiConfig()
	if cfg == nil || cfg.BaseURL == "" || cfg.APIKey == "" || cfg.Model == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	bioDisplay := bio
	if bioDisplay == "" {
		bioDisplay = "(none)"
	}

	result, err := ai.AssessUser(ctx, *cfg, displayName, username, bio)
	if err != nil {
		m.broker.Publish(logstream.Entry{
			Timestamp: time.Now(),
			Level:     "WARN",
			Category:  "AI_ASSESS",
			ChatID:    chatID,
			UserID:    userID,
			Username:  username,
			IsNew:     true,
			Message: fmt.Sprintf("AI assessment failed for %s (@%s): %v (%.1fs)",
				userRef(userID, displayName), username, err, 0.0),
		})
		return
	}

	level := "INFO"
	category := "AI_ASSESS"
	isSpam := result.RiskLevel == "确认spam" || result.RiskLevel == "高风险"
	if result.RiskLevel == "确认spam" {
		level = "WARN"
		category = "SPAM_CONFIRMED"
	} else if result.RiskLevel == "高风险" {
		level = "WARN"
		category = "SPAM_HIGH_RISK"
	}

	m.broker.Publish(logstream.Entry{
		Timestamp: time.Now(),
		Level:     level,
		Category:  category,
		ChatID:    chatID,
		UserID:    userID,
		Username:  username,
		IsNew:     true,
		Message: fmt.Sprintf("AI spam risk for %s (@%s, Bio: %s): %s — %s (%.1fs)",
			userRef(userID, displayName), username, bioDisplay,
			result.RiskLevel, result.Reason,
			result.Duration.Seconds()),
	})

	if isSpam && m.warnInGroup != nil && m.warnInGroup() {
		m.sendSpamWarning(chatID, userID, displayName, username)
	}
}

func (m *Monitor) sendSpamWarning(chatID int64, userID int64, displayName, username string) {
	b := m.bot.Load()
	if b == nil {
		return
	}

	mention := displayName
	if username != "" {
		mention = "@" + username
	}
	text := fmt.Sprintf("检测到spam账号 %s 入群，建议立即移除。", mention)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sent, err := b.SendMessage(ctx, &telego.SendMessageParams{
		ChatID: telego.ChatID{ID: chatID},
		Text:   text,
	})
	if err != nil {
		m.broker.Publish(logstream.Entry{
			Timestamp: time.Now(),
			Level:     "WARN",
			Category:  "SPAM_WARNING",
			ChatID:    chatID,
			UserID:    userID,
			Username:  username,
			IsNew:     true,
			Message:   fmt.Sprintf("Failed to send spam warning for %s: %v", userRef(userID, displayName), err),
		})
		return
	}

	m.broker.Publish(logstream.Entry{
		Timestamp: time.Now(),
		Level:     "INFO",
		Category:  "SPAM_WARNING",
		ChatID:    chatID,
		UserID:    userID,
		Username:  username,
		IsNew:     true,
		Message:   fmt.Sprintf("Spam warning sent for %s, will auto-delete in 120s", userRef(userID, displayName)),
	})

	go m.scheduleDeleteMessage(chatID, sent.MessageID, 120*time.Second)
}

func (m *Monitor) scheduleDeleteMessage(chatID int64, messageID int, delay time.Duration) {
	time.Sleep(delay)

	b := m.bot.Load()
	if b == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = b.DeleteMessage(ctx, &telego.DeleteMessageParams{
		ChatID:    telego.ChatID{ID: chatID},
		MessageID: messageID,
	})
}

func (m *Monitor) recordJoin(chatID int64) {
	m.floodMu.Lock()
	defer m.floodMu.Unlock()

	now := time.Now()
	m.floodJoins[chatID] = append(m.floodJoins[chatID], now)
	m.floodLastJoin[chatID] = now

	cutoff := now.Add(-1 * time.Minute)
	var recent []time.Time
	for _, t := range m.floodJoins[chatID] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	m.floodJoins[chatID] = recent

	if !m.floodActive[chatID] && len(recent) >= 3 {
		m.floodActive[chatID] = true
		m.broker.Publish(logstream.Entry{
			Timestamp: now,
			Level:     "WARN",
			Category:  "FLOOD_ALERT",
			ChatID:    chatID,
			Tags:      []string{"FLOOD"},
			Message:   fmt.Sprintf("Flood detected in chat %d: %d joins within 1 minute", chatID, len(recent)),
		})
		go m.sendFloodWarning(chatID)
	}
}

func (m *Monitor) isFloodActive(chatID int64) bool {
	m.floodMu.Lock()
	defer m.floodMu.Unlock()
	return m.floodActive[chatID]
}

func (m *Monitor) startFloodCleanup() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		m.floodMu.Lock()
		now := time.Now()
		for chatID, lastJoin := range m.floodLastJoin {
			if now.Sub(lastJoin) > 1*time.Minute {
				wasActive := m.floodActive[chatID]
				delete(m.floodJoins, chatID)
				delete(m.floodActive, chatID)
				delete(m.floodLastJoin, chatID)
				if wasActive {
					m.broker.Publish(logstream.Entry{
						Timestamp: now,
						Level:     "INFO",
						Category:  "FLOOD_END",
						ChatID:    chatID,
						Tags:      []string{"FLOOD"},
						Message:   fmt.Sprintf("Flood mode ended for chat %d: no joins for 1 minute", chatID),
					})
				}
			}
		}
		m.floodMu.Unlock()
	}
}

func (m *Monitor) sendFloodWarning(chatID int64) {
	if m.warnInGroup == nil || !m.warnInGroup() {
		return
	}

	b := m.bot.Load()
	if b == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	text := "⚠️ 检测到正在遭受SPAM洪流攻击，建议临时禁止入群。"
	sent, err := b.SendMessage(ctx, &telego.SendMessageParams{
		ChatID: telego.ChatID{ID: chatID},
		Text:   text,
	})
	if err != nil {
		m.broker.Publish(logstream.Entry{
			Timestamp: time.Now(),
			Level:     "WARN",
			Category:  "FLOOD_ALERT",
			ChatID:    chatID,
			Tags:      []string{"FLOOD"},
			Message:   fmt.Sprintf("Failed to send flood warning to chat %d: %v", chatID, err),
		})
		return
	}

	m.broker.Publish(logstream.Entry{
		Timestamp: time.Now(),
		Level:     "INFO",
		Category:  "FLOOD_ALERT",
		ChatID:    chatID,
		Tags:      []string{"FLOOD"},
		Message:   fmt.Sprintf("Flood warning sent to chat %d, will auto-delete in 120s", chatID),
	})

	go m.scheduleDeleteMessage(chatID, sent.MessageID, 120*time.Second)
}

func (m *Monitor) processMessage(msg *telego.Message, source string) {
	chatID := msg.Chat.ID
	if !m.isMonitored(chatID) {
		return
	}

	if m.updateTitle != nil && msg.Chat.Title != "" {
		m.updateTitle(chatID, msg.Chat.Title)
	}

	if len(msg.NewChatMembers) > 0 {
		for _, member := range msg.NewChatMembers {
			displayName := memberDisplayName(member)

			if !member.IsBot {
				bio := m.fetchUserBio(member.ID)
				m.markNewUser(member.ID, chatID, displayName, member.Username, bio)

				bioDisplay := bio
				if bioDisplay == "" {
					bioDisplay = "(none)"
				}
				tgDate := time.Unix(int64(msg.Date), 0)
				m.broker.Publish(logstream.Entry{
					Timestamp:    time.Now(),
					TelegramDate: &tgDate,
					Level:        "INFO",
					Category:     "JOIN",
					Source:       source,
					ChatID:       chatID,
					UserID:       member.ID,
					Username:     member.Username,
					IsNew:        true,
					Message: fmt.Sprintf("New member joined: %s (@%s, Bio: %s)",
						userRef(member.ID, displayName), member.Username, bioDisplay),
				})
			} else {
				tgDate := time.Unix(int64(msg.Date), 0)
				m.broker.Publish(logstream.Entry{
					Timestamp:    time.Now(),
					TelegramDate: &tgDate,
					Level:        "WARN",
					Category:     "BOT_JOIN",
					Source:       source,
					ChatID:       chatID,
					UserID:       member.ID,
					Username:     member.Username,
					Message:      fmt.Sprintf("Bot added to group: %s", userRef(member.ID, "@"+member.Username)),
				})
			}
		}
	}

	if msg.LeftChatMember != nil {
		member := msg.LeftChatMember
		tgDate := time.Unix(int64(msg.Date), 0)
		m.broker.Publish(logstream.Entry{
			Timestamp:    time.Now(),
			TelegramDate: &tgDate,
			Level:        "INFO",
			Category:     "LEAVE",
			Source:       source,
			ChatID:       chatID,
			UserID:       member.ID,
			Username:     member.Username,
			Message:      fmt.Sprintf("Member left: %s", userRef(member.ID, memberDisplayName(*member))),
		})
	}

	if msg.From == nil {
		return
	}

	entityTags := collectEntityTags(msg)
	quoteInfo := extractQuoteInfo(msg)

	isBot := msg.From.IsBot
	isVerifyBot := isBot && m.isVerifyBot(msg.From.ID)
	isNew := !isBot && m.isNewUser(msg.From.ID)

	mutualCount := 0
	if !isBot {
		mutualCount = m.countMutualGroups(msg.From.ID)
	}
	isZeroMutual := !isBot && mutualCount == 0

	if !isBot && !isNew {
		entityTags = nil
	}
	hasEntities := len(entityTags) > 0
	hasQuote := quoteInfo != ""

	if !isBot && !isNew && !hasEntities && !isZeroMutual {
		return
	}

	var category, level string
	var tags []string
	switch {
	case isVerifyBot:
		category, level = "BOT_MSG", "INFO"
		tags = []string{"BOT_OP"}
	case isBot:
		category, level = "BOT_MSG", "INFO"
	case isZeroMutual:
		category, level = "SPAM_CONFIRMED", "WARN"
		tags = []string{"ZERO_MG"}
	case isNew:
		category, level = "NEW_MSG", "INFO"
	case hasEntities:
		category, level = entityTags[0], "INFO"
	default:
		category, level = "QUOTE", "INFO"
	}

	content := extractText(msg)
	if content == "" {
		content = describeMedia(msg)
	}

	var parts []string
	for _, tag := range entityTags {
		parts = append(parts, "["+tag+"]")
	}
	parts = append(parts, userRef(msg.From.ID, memberDisplayName(*msg.From))+":")
	parts = append(parts, truncate(content, 300))
	if hasQuote {
		parts = append(parts, quoteInfo)
	}

	if !isBot && (isNew || isZeroMutual) {
		parts = append(parts, fmt.Sprintf("(MG:%d)", mutualCount))
	}

	tgDate := time.Unix(int64(msg.Date), 0)
	m.broker.Publish(logstream.Entry{
		Timestamp:    time.Now(),
		TelegramDate: &tgDate,
		Level:        level,
		Category:     category,
		Tags:         tags,
		Source:       source,
		ChatID:       chatID,
		UserID:       msg.From.ID,
		Username:     msg.From.Username,
		IsNew:        isNew,
		MutualGroups: mutualCount,
		Message:      strings.Join(parts, " "),
		Raw:          content,
	})
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

func collectEntityTags(msg *telego.Message) []string {
	entities := msg.Entities
	if len(entities) == 0 {
		entities = msg.CaptionEntities
	}
	if len(entities) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	var tags []string
	for _, e := range entities {
		var tag string
		switch e.Type {
		case telego.EntityTypeURL:
			tag = "URL_ENTITY"
		case telego.EntityTypeTextLink:
			tag = "TEXT_LINK"
		case telego.EntityTypeMention:
			tag = "MENTION"
		case telego.EntityTypeHashtag:
			tag = "HASHTAG"
		case telego.EntityTypeBotCommand:
			tag = "BOT_COMMAND"
		default:
			continue
		}
		if !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	return tags
}

func extractQuoteInfo(msg *telego.Message) string {
	hasQuote := msg.Quote != nil && msg.Quote.Text != ""
	hasReply := msg.ReplyToMessage != nil

	if !hasQuote && !hasReply {
		return ""
	}

	var parts []string

	if hasQuote {
		parts = append(parts, fmt.Sprintf("[Quote: %s]", truncate(msg.Quote.Text, 100)))
	}

	if hasReply {
		reply := msg.ReplyToMessage
		replyFrom := "unknown"
		if reply.From != nil {
			replyFrom = userRef(reply.From.ID, memberDisplayName(*reply.From))
		}
		replyText := extractText(reply)
		if replyText != "" {
			parts = append(parts, fmt.Sprintf("[Reply to %s: %s]", replyFrom, truncate(replyText, 100)))
		} else if reply.Photo != nil {
			parts = append(parts, fmt.Sprintf("[Reply to %s: <photo>]", replyFrom))
		} else if reply.Document != nil {
			parts = append(parts, fmt.Sprintf("[Reply to %s: <document>]", replyFrom))
		} else if reply.Video != nil {
			parts = append(parts, fmt.Sprintf("[Reply to %s: <video>]", replyFrom))
		} else if reply.Sticker != nil {
			parts = append(parts, fmt.Sprintf("[Reply to %s: <sticker>]", replyFrom))
		} else {
			parts = append(parts, fmt.Sprintf("[Reply to %s: <non-text>]", replyFrom))
		}
	}

	return strings.Join(parts, " ")
}

func (m *Monitor) countMutualGroups(userID int64) int {
	b := m.bot.Load()
	if b == nil {
		return 0
	}
	groups := m.monitored()
	if len(groups) == 0 {
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	count := 0
	for groupID := range groups {
		member, err := b.GetChatMember(ctx, &telego.GetChatMemberParams{
			ChatID: telego.ChatID{ID: groupID},
			UserID: userID,
		})
		if err != nil {
			continue
		}
		status := member.MemberStatus()
		if status != telego.MemberStatusLeft && status != telego.MemberStatusBanned {
			count++
		}
	}
	return count
}

func describeMedia(msg *telego.Message) string {
	if msg.Photo != nil {
		return "<photo>"
	}
	if msg.Video != nil {
		return "<video>"
	}
	if msg.Document != nil {
		return "<document>"
	}
	if msg.Sticker != nil {
		return "<sticker>"
	}
	if msg.Voice != nil {
		return "<voice>"
	}
	if msg.VideoNote != nil {
		return "<video_note>"
	}
	if msg.Animation != nil {
		return "<gif>"
	}
	return "<non-text>"
}

func (m *Monitor) processChatMemberUpdate(update *telego.ChatMemberUpdated) {
	chatID := update.Chat.ID
	if !m.isMonitored(chatID) {
		return
	}

	if m.updateTitle != nil && update.Chat.Title != "" {
		m.updateTitle(chatID, update.Chat.Title)
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
		performedBy = userRef(update.From.ID, memberDisplayName(update.From))
		actorIsVerifyBot = m.isVerifyBot(update.From.ID)
	}

	targetUser := newMember.MemberUser()

	switch {
	case status == telego.MemberStatusMember && update.From.ID == targetUser.ID:
		m.markNewUser(targetUser.ID, chatID, memberDisplayName(targetUser), targetUser.Username, "")
	case status == telego.MemberStatusRestricted && update.From.IsBot:
		if r, ok := newMember.(*telego.ChatMemberRestricted); ok && !r.CanSendMessages {
			m.markNewUser(targetUser.ID, chatID, memberDisplayName(targetUser), targetUser.Username, "")
		}
	}

	targetIsNew := m.isNewUser(targetUser.ID)

	var tags []string
	if actorIsVerifyBot && targetIsNew {
		tags = []string{"BOT_OP"}
	}

	tgDate := time.Unix(int64(update.Date), 0)
	m.broker.Publish(logstream.Entry{
		Timestamp:    time.Now(),
		TelegramDate: &tgDate,
		Level:        level,
		Category:     category,
		Tags:         tags,
		ChatID:       chatID,
		UserID:       targetUser.ID,
		Username:     targetUser.Username,
		IsNew:        targetIsNew,
		Message: fmt.Sprintf("%s was %s by %s",
			userRef(targetUser.ID, memberDisplayName(targetUser)), action, performedBy),
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
	var tags []string
	if isNew {
		if msg := cq.Message.Message(); msg != nil && msg.From.IsBot && m.isVerifyBot(msg.From.ID) {
			tags = []string{"BOT_OP"}
		}
	}

	m.broker.Publish(logstream.Entry{
		Timestamp: time.Now(),
		Level:     "INFO",
		Category:  category,
		Tags:      tags,
		ChatID:    chatID,
		UserID:    from.ID,
		Username:  from.Username,
		IsNew:     isNew,
		Message: fmt.Sprintf("Button click by %s: %q",
			userRef(from.ID, memberDisplayName(from)), cq.Data),
		Raw: cq.Data,
	})
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

func userRef(id int64, name string) string {
	return fmt.Sprintf("[U:%d:%s]", id, name)
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}


