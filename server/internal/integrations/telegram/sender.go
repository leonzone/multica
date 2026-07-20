package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// User-facing copy the bot speaks in Telegram. Chinese-first, aligned with
// the Lark adapter's product voice (conventions.zh.mdx); the binding prompt
// carries the redeem link.
const (
	msgAgentOffline    = "⚠️ 智能体当前离线，消息已记录。下次 daemon 上线后会自动继续处理。"
	msgAgentArchived   = "⚠️ 该智能体已归档，无法回复。请联系工作区管理员。"
	msgUnsupportedType = "暂不支持此类消息，请发送文字内容。"
)

// maxMessageRunes caps one outbound sendMessage body. Telegram hard-caps a
// message at 4096 UTF-16 code units; 3500 runes leaves headroom for HTML tags
// added by the markdown conversion.
const maxMessageRunes = 3500

// sender posts agent replies back to Telegram via sendMessage. Outbound half
// only; the installation identity is resolved per message by the Router.
type sender struct {
	api    *botAPI
	logger *slog.Logger
}

func newSender(api *botAPI, logger *slog.Logger) *sender {
	if logger == nil {
		logger = slog.Default()
	}
	return &sender{api: api, logger: logger}
}

// Send delivers a text reply, converting Markdown to Telegram HTML and
// chunking under the per-message cap. The returned SendResult carries the id
// of the LAST posted chunk. A rejected HTML payload falls back to plain text
// so a conversion edge case can never eat the reply.
func (s *sender) Send(ctx context.Context, out channel.OutboundMessage) (channel.SendResult, error) {
	if s.api == nil {
		return channel.SendResult{}, errors.New("telegram: api client not configured")
	}
	chatID, err := strconv.ParseInt(out.ChatID, 10, 64)
	if err != nil {
		return channel.SendResult{}, fmt.Errorf("telegram: bad chat id %q: %w", out.ChatID, err)
	}
	var threadID int64
	if out.ThreadID != "" {
		threadID, _ = strconv.ParseInt(out.ThreadID, 10, 64)
	}
	var replyTo int64
	if out.ReplyTo != "" {
		replyTo = parseMessageRef(out.ReplyTo)
	}

	var lastID string
	for _, chunk := range chunkMessage(out.Text, maxMessageRunes) {
		m, err := s.api.SendMessage(ctx, sendMessageParams{
			ChatID:           chatID,
			Text:             formatHTML(chunk),
			ParseMode:        "HTML",
			MessageThreadID:  threadID,
			ReplyToMessageID: replyTo,
		})
		if err != nil {
			// HTML rejection fallback: send the raw markdown as plain text.
			m, err = s.api.SendMessage(ctx, sendMessageParams{
				ChatID:           chatID,
				Text:             chunk,
				MessageThreadID:  threadID,
				ReplyToMessageID: replyTo,
			})
			if err != nil {
				return channel.SendResult{}, fmt.Errorf("telegram: sendMessage: %w", err)
			}
		}
		lastID = messageKey(chatID, m.MessageID)
		replyTo = 0 // only the first chunk quotes
	}
	return channel.SendResult{MessageID: lastID}, nil
}

// parseMessageRef extracts the numeric message id from either a bare id or
// the composite "chat:message" key this adapter stores.
func parseMessageRef(ref string) int64 {
	if _, after, ok := strings.Cut(ref, ":"); ok {
		ref = after
	}
	id, _ := strconv.ParseInt(ref, 10, 64)
	return id
}

// chunkMessage splits text into <=maxRunes pieces on rune boundaries,
// preferring newline breaks so code blocks and paragraphs split cleanly.
func chunkMessage(text string, maxRunes int) []string {
	runes := []rune(text)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return []string{text}
	}
	var chunks []string
	for len(runes) > 0 {
		n := maxRunes
		if n > len(runes) {
			n = len(runes)
		} else {
			// Prefer the last newline inside the window.
			window := runes[:n]
			if i := lastIndexRune(window, '\n'); i > maxRunes/2 {
				n = i + 1
			}
		}
		chunks = append(chunks, strings.TrimRight(string(runes[:n]), "\n"))
		runes = runes[n:]
	}
	return chunks
}

func lastIndexRune(rs []rune, r rune) int {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i] == r {
			return i
		}
	}
	return -1
}
