package telegram

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// This file holds the translation from a Telegram Update to the engine's
// normalized channel.InboundMessage. Free functions parameterized by the bot
// identity, mirroring slack/inbound.go, so the per-installation polling loop
// threads in its own bot's id and username.

// telegramRawEvent carries the Telegram-specific fields the cross-platform
// envelope does not — read back only inside the Telegram resolvers.
type telegramRawEvent struct {
	// BotID routes the message to its installation (config->>'app_id').
	BotID string `json:"bot_id"`
	// EventType is a coarse label for drop audits ("message").
	EventType string `json:"event_type"`
	// SenderName is the sender's Telegram display name, carried for
	// group-context attribution.
	SenderName string `json:"sender_name,omitempty"`
}

// freshCommand is the "start a fresh agent session" trigger, aligned with the
// Feishu adapter's /fresh affordance. Telegram clients may suffix commands
// with @botname in groups.
const freshCommand = "/fresh"

// inboundFromUpdate normalizes one Telegram update. ok=false means the update
// must not reach the core: bot/self messages, channel posts, edits (excluded
// via allowed_updates already), or unsupported media (the caller decides
// whether to send an "unsupported" notice for p2p).
//
// Group addressing policy mirrors Slack v1: a group message is addressed to
// the bot only when it carries an explicit @bot mention or directly replies to
// one of the bot's messages. Privacy mode is left ON, so Telegram already
// withholds unaddressed group chatter from the bot; this check is the
// defense-in-depth for bots whose privacy mode was disabled in BotFather.
func inboundFromUpdate(u Update, botID int64, botUsername string) (channel.InboundMessage, bool) {
	m := u.Message
	if m == nil || m.From == nil || m.From.IsBot || m.From.ID == botID {
		return channel.InboundMessage{}, false
	}
	chatType, ok := telegramChatType(m.Chat.Type)
	if !ok {
		return channel.InboundMessage{}, false
	}

	text := m.Text
	if text == "" {
		text = m.Caption
	}
	msgType := classifyMessage(m)

	mentioned := mentionsBot(m, botUsername)
	repliedToBot := m.ReplyToMessage != nil && m.ReplyToMessage.From != nil && m.ReplyToMessage.From.ID == botID
	addressed := chatType == channel.ChatTypeP2P || mentioned || repliedToBot

	cleaned, forceFresh := normalizeText(text, botUsername)

	senderID := strconv.FormatInt(m.From.ID, 10)
	chatID := strconv.FormatInt(m.Chat.ID, 10)
	threadID := ""
	if m.IsTopicMessage && m.MessageThreadID != 0 {
		threadID = strconv.FormatInt(m.MessageThreadID, 10)
	}

	raw, _ := json.Marshal(telegramRawEvent{
		BotID:      strconv.FormatInt(botID, 10),
		EventType:  "message",
		SenderName: senderDisplayName(m.From),
	})

	var reply *channel.ReplyCtx
	if m.ReplyToMessage != nil {
		reply = &channel.ReplyCtx{
			MessageID: messageKey(m.Chat.ID, m.ReplyToMessage.MessageID),
			RootID:    threadID,
		}
	}

	return channel.InboundMessage{
		EventID: strconv.FormatInt(u.UpdateID, 10),
		// Telegram message ids are only unique per chat, so the dedup key
		// (installation, message_id) uses the composite chat:message form.
		MessageID:      messageKey(m.Chat.ID, m.MessageID),
		Type:           msgType,
		Text:           cleaned,
		ReplyTo:        reply,
		AddressedToBot: addressed,
		ForceFresh:     forceFresh,
		Source: channel.Source{
			ChannelType: TypeTelegram,
			ChatID:      chatID,
			ChatType:    chatType,
			SenderID:    senderID,
			// Telegram user ids are global, so the per-installation id doubles as
			// the cross-installation stable id.
			SenderStableID: senderID,
			ThreadID:       threadID,
		},
		Raw: raw,
	}, true
}

// messageKey builds the per-installation-unique message id "chat:message".
func messageKey(chatID, messageID int64) string {
	return strconv.FormatInt(chatID, 10) + ":" + strconv.FormatInt(messageID, 10)
}

// telegramChatType maps Telegram's chat.type. Channel posts (broadcast
// channels, no interactive sender context) are not ingested.
func telegramChatType(t string) (channel.ChatType, bool) {
	switch t {
	case "private":
		return channel.ChatTypeP2P, true
	case "group", "supergroup":
		return channel.ChatTypeGroup, true
	default:
		return "", false
	}
}

// classifyMessage maps the message payload to the normalized MsgType. Only
// text is actionable in v1 (aligned with Feishu/Slack); media kinds are
// reported so the caller can reply "unsupported" rather than stay silent.
func classifyMessage(m *Message) channel.MsgType {
	switch {
	case m.Text != "":
		return channel.MsgTypeText
	case len(m.Photo) > 0:
		return channel.MsgTypeImage
	case m.Voice != nil:
		return channel.MsgTypeAudio
	case m.Video != nil:
		return channel.MsgTypeVideo
	case m.Document != nil:
		return channel.MsgTypeFile
	default:
		return channel.MsgTypeUnknown
	}
}

// mentionsBot reports whether the message text contains "@botusername".
// Telegram marks mentions with entities, but matching the literal token is
// equivalent for bot usernames (they are globally unique and always start
// with "@" in text) and keeps the check entity-order independent.
func mentionsBot(m *Message, botUsername string) bool {
	if botUsername == "" {
		return false
	}
	return containsFold(m.Text, "@"+botUsername) || containsFold(m.Caption, "@"+botUsername)
}

// normalizeText strips the bot mention token and the optional /fresh command
// prefix, returning the cleaned prompt and the force-fresh flag. Command
// suffix forms ("/fresh@my_bot") are handled.
func normalizeText(text, botUsername string) (string, bool) {
	cleaned := text
	if botUsername != "" {
		cleaned = replaceFold(cleaned, "@"+botUsername)
	}
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == freshCommand || strings.HasPrefix(cleaned, freshCommand+" ") {
		return strings.TrimSpace(strings.TrimPrefix(cleaned, freshCommand)), true
	}
	return cleaned, false
}

// senderDisplayName renders "First Last" or the username as fallback.
func senderDisplayName(u *User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name != "" {
		return name
	}
	return u.Username
}

// containsFold is a case-insensitive strings.Contains (bot usernames are
// case-insensitive on Telegram).
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// replaceFold removes every case-insensitive occurrence of sub from s.
func replaceFold(s, sub string) string {
	lower := strings.ToLower(s)
	lowerSub := strings.ToLower(sub)
	var b strings.Builder
	for {
		i := strings.Index(lower, lowerSub)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		s = s[i+len(sub):]
		lower = lower[i+len(lowerSub):]
	}
}
