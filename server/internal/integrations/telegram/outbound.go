package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Outbound delivers an agent's chat reply back to Telegram — the outbound
// half of the round trip, mirroring lark.Patcher / slack.Outbound on the
// shared event bus.
//
// Streaming: Telegram has no stream-update protocol, so the "stream 帧" UX is
// simulated with the platform's canonical pattern — post one placeholder
// message on the first partial, then throttled editMessageText calls as the
// agent's transcript grows (EventTaskMessage text frames), and a final edit /
// send on EventChatDone. Edits are throttled per chat to stay inside
// Telegram's editMessageText rate budget; on a 429 the streamer backs off and
// the final content always lands via the EventChatDone path.
type Outbound struct {
	q       outboundQueries
	decrypt Decrypter
	logger  *slog.Logger
	apiBase string
	client  *http.Client

	mu      sync.Mutex
	streams map[string]*streamState // key = chat_session_id
}

// outboundQueries is the slice of generated queries the subscriber needs.
// *db.Queries satisfies it.
type outboundQueries interface {
	GetAgentTask(ctx context.Context, id pgtype.UUID) (db.AgentTaskQueue, error)
	GetChannelChatSessionBindingBySession(ctx context.Context, arg db.GetChannelChatSessionBindingBySessionParams) (db.ChannelChatSessionBinding, error)
	GetChannelInstallation(ctx context.Context, arg db.GetChannelInstallationParams) (db.ChannelInstallation, error)
}

// streamState tracks one in-flight streamed reply.
type streamState struct {
	chatID      int64
	threadID    int64
	messageID   int64 // placeholder message being edited; 0 until first send
	accumulated string
	lastEdit    time.Time
	backoffTill time.Time
}

// editInterval is the minimum spacing between editMessageText calls per chat.
// Telegram tolerates roughly one edit per second per chat, with a much
// stricter per-group budget (~20 messages/min); 2.5s keeps a long generation
// well inside both without feeling static.
const editInterval = 2500 * time.Millisecond

// streamPlaceholder is the first frame's text while the first tokens arrive.
const streamPlaceholder = "…"

// taskFailedText is sent when the agent run fails outright.
const taskFailedText = "❌ 智能体处理失败，请稍后重试。"

// NewOutbound builds the Telegram outbound subscriber.
func NewOutbound(q outboundQueries, decrypt Decrypter, apiBase string, client *http.Client, logger *slog.Logger) *Outbound {
	if logger == nil {
		logger = slog.Default()
	}
	return &Outbound{
		q:       q,
		decrypt: decrypt,
		logger:  logger,
		apiBase: apiBase,
		client:  client,
		streams: make(map[string]*streamState),
	}
}

// Register subscribes to the transcript / completion / failure events.
func (o *Outbound) Register(bus *events.Bus) {
	bus.Subscribe(protocol.EventTaskMessage, o.handleTaskMessage)
	bus.Subscribe(protocol.EventChatDone, o.handleChatDone)
	bus.Subscribe(protocol.EventTaskFailed, o.handleTaskFailed)
}

// handleTaskMessage streams a partial: on each agent text frame, update the
// placeholder message (throttled). Bus delivery is synchronous, so all work
// runs under a tight timeout and never propagates errors.
func (o *Outbound) handleTaskMessage(e events.Event) {
	payload, ok := e.Payload.(protocol.TaskMessagePayload)
	if !ok || payload.Type != "text" || payload.Content == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	target, err := o.resolveTarget(ctx, e, true)
	if err != nil || target == nil {
		return
	}

	o.mu.Lock()
	st, exists := o.streams[target.sessionKey]
	if !exists {
		st = &streamState{chatID: target.chatID, threadID: target.threadID}
		o.streams[target.sessionKey] = st
	}
	st.accumulated += payload.Content
	now := time.Now()
	throttled := now.Before(st.backoffTill) || now.Sub(st.lastEdit) < editInterval
	snapshot := st.accumulated
	msgID := st.messageID
	if !throttled {
		st.lastEdit = now
	}
	o.mu.Unlock()

	if throttled {
		return
	}
	o.pushPartial(ctx, target, st, msgID, snapshot)
}

// pushPartial sends the placeholder on the first flush and edits it after.
func (o *Outbound) pushPartial(ctx context.Context, target *replyTarget, st *streamState, msgID int64, snapshot string) {
	api := newBotAPI(o.apiBase, target.botToken, o.client)
	text := snapshot
	if len([]rune(text)) > maxMessageRunes {
		// Mid-stream overflow: freeze the streamed message at the cap; the full
		// reply is delivered in chunks by the final EventChatDone send.
		text = string([]rune(text)[:maxMessageRunes])
	}
	if msgID == 0 {
		m, err := api.SendMessage(ctx, sendMessageParams{
			ChatID:          st.chatID,
			Text:            firstNonEmpty(formatHTML(text), streamPlaceholder),
			ParseMode:       "HTML",
			MessageThreadID: st.threadID,
		})
		if err != nil {
			o.noteEditFailure(st, err)
			return
		}
		o.mu.Lock()
		st.messageID = m.MessageID
		o.mu.Unlock()
		return
	}
	err := api.EditMessageText(ctx, editMessageTextParams{
		ChatID:    st.chatID,
		MessageID: msgID,
		Text:      formatHTML(text),
		ParseMode: "HTML",
	})
	if err != nil && !isNotModified(err) {
		o.noteEditFailure(st, err)
	}
}

// noteEditFailure applies the 429-mandated backoff to the stream; other
// failures are logged and the stream simply stops editing (the final content
// still lands via EventChatDone).
func (o *Outbound) noteEditFailure(st *streamState, err error) {
	if wait, ok := retryAfter(err); ok {
		o.mu.Lock()
		st.backoffTill = time.Now().Add(wait)
		o.mu.Unlock()
		return
	}
	o.logger.Warn("telegram outbound: stream edit failed", "error", err)
}

// handleChatDone finalizes: edit the streamed message to the final content
// (chunking overflow into follow-up messages), or send fresh if nothing was
// streamed.
func (o *Outbound) handleChatDone(e events.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := o.finishChat(ctx, e); err != nil {
		o.logger.WarnContext(ctx, "telegram outbound: reply delivery failed",
			"error", err, "chat_session_id", e.ChatSessionID)
	}
}

func (o *Outbound) finishChat(ctx context.Context, e events.Event) error {
	target, err := o.resolveTarget(ctx, e, false)
	if err != nil {
		return err
	}
	if target == nil {
		return nil // not a Telegram session
	}
	content := chatDoneContent(e.Payload)

	o.mu.Lock()
	st := o.streams[target.sessionKey]
	delete(o.streams, target.sessionKey)
	o.mu.Unlock()

	if content == "" {
		return nil
	}
	api := newBotAPI(o.apiBase, target.botToken, o.client)
	chunks := chunkMessage(content, maxMessageRunes)

	start := 0
	if st != nil && st.messageID != 0 {
		// Rewrite the streamed placeholder with the first (or only) final chunk.
		if err := api.EditMessageText(ctx, editMessageTextParams{
			ChatID:    st.chatID,
			MessageID: st.messageID,
			Text:      formatHTML(chunks[0]),
			ParseMode: "HTML",
		}); err != nil && !isNotModified(err) {
			// Fall through to a fresh send of ALL chunks so the reply is never lost.
			start = 0
		} else {
			start = 1
		}
	}
	sender := newSender(api, o.logger)
	for _, chunk := range chunks[start:] {
		if _, err := sender.Send(ctx, channel.OutboundMessage{
			ChatID:   strconv.FormatInt(target.chatID, 10),
			Text:     chunk,
			ThreadID: threadIDString(target.threadID),
		}); err != nil {
			return fmt.Errorf("send final chunk: %w", err)
		}
	}
	return nil
}

// handleTaskFailed clears any stream state and posts a failure notice.
func (o *Outbound) handleTaskFailed(e events.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	target, err := o.resolveTarget(ctx, e, false)
	if err != nil || target == nil {
		return
	}
	o.mu.Lock()
	st := o.streams[target.sessionKey]
	delete(o.streams, target.sessionKey)
	o.mu.Unlock()

	api := newBotAPI(o.apiBase, target.botToken, o.client)
	if st != nil && st.messageID != 0 {
		if err := api.EditMessageText(ctx, editMessageTextParams{
			ChatID:    st.chatID,
			MessageID: st.messageID,
			Text:      taskFailedText,
		}); err == nil || isNotModified(err) {
			return
		}
	}
	if _, err := api.SendMessage(ctx, sendMessageParams{
		ChatID:          target.chatID,
		Text:            taskFailedText,
		MessageThreadID: target.threadID,
	}); err != nil {
		o.logger.WarnContext(ctx, "telegram outbound: failure notice failed", "error", err)
	}
}

// replyTarget is the resolved destination for one event.
type replyTarget struct {
	sessionKey string
	chatID     int64
	threadID   int64
	botToken   string
}

// resolveTarget maps an event to its Telegram binding + credentials. Returns
// (nil, nil) when the session is not Telegram-bound. When viaTask is true the
// chat session id is recovered from the task row (EventTaskMessage carries
// only TaskID).
func (o *Outbound) resolveTarget(ctx context.Context, e events.Event, viaTask bool) (*replyTarget, error) {
	sessionID, err := util.ParseUUID(e.ChatSessionID)
	if err != nil || !sessionID.Valid {
		if !viaTask {
			return nil, nil
		}
		taskID, terr := util.ParseUUID(e.TaskID)
		if terr != nil || !taskID.Valid {
			return nil, nil
		}
		task, terr := o.q.GetAgentTask(ctx, taskID)
		if terr != nil || !task.ChatSessionID.Valid {
			return nil, nil
		}
		sessionID = task.ChatSessionID
	}
	binding, err := o.q.GetChannelChatSessionBindingBySession(ctx, db.GetChannelChatSessionBindingBySessionParams{
		ChatSessionID: sessionID,
		ChannelType:   string(TypeTelegram),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // not a Telegram session
		}
		return nil, fmt.Errorf("lookup telegram chat binding: %w", err)
	}
	// Tasks triggered from web/mobile on a Telegram-originated session reply
	// only in Multica (chat_input_task_id set) — same fail-closed origin rule
	// as Slack.
	if taskID, ok := eventTaskID(e); ok {
		task, terr := o.q.GetAgentTask(ctx, taskID)
		if terr == nil && task.ChatInputTaskID.Valid {
			return nil, nil
		}
	}
	inst, err := o.q.GetChannelInstallation(ctx, db.GetChannelInstallationParams{
		ID:          binding.InstallationID,
		ChannelType: string(TypeTelegram),
	})
	if err != nil {
		return nil, fmt.Errorf("load telegram installation: %w", err)
	}
	if inst.Status != "active" {
		return nil, nil // revoked between trigger and reply
	}
	creds, err := decodeCredentials(inst.Config, o.decrypt)
	if err != nil {
		return nil, fmt.Errorf("decode telegram credentials: %w", err)
	}
	chatID, threadID := outboundTarget(binding)
	return &replyTarget{
		sessionKey: util.UUIDToString(sessionID),
		chatID:     chatID,
		threadID:   threadID,
		botToken:   creds.BotToken,
	}, nil
}

// outboundTarget recovers the numeric chat id (from the binding config when
// the binding key is a composite "chat:thread") and the reply thread.
func outboundTarget(b db.ChannelChatSessionBinding) (chatID, threadID int64) {
	raw := b.ChannelChatID
	if len(b.Config) > 0 {
		var cfg telegramBindingConfig
		if err := json.Unmarshal(b.Config, &cfg); err == nil && cfg.ChatID != "" {
			raw = cfg.ChatID
		}
	}
	chatID, _ = strconv.ParseInt(raw, 10, 64)
	if b.LastThreadID.Valid {
		threadID, _ = strconv.ParseInt(b.LastThreadID.String, 10, 64)
	}
	return chatID, threadID
}

// eventTaskID extracts the task id from the event envelope or payload.
func eventTaskID(e events.Event) (pgtype.UUID, bool) {
	raw := e.TaskID
	if raw == "" {
		switch p := e.Payload.(type) {
		case protocol.ChatDonePayload:
			raw = p.TaskID
		case map[string]any:
			raw, _ = p["task_id"].(string)
		}
	}
	id, err := util.ParseUUID(raw)
	return id, err == nil && id.Valid
}

// chatDoneContent extracts the reply text from an EventChatDone payload.
func chatDoneContent(payload any) string {
	switch p := payload.(type) {
	case protocol.ChatDonePayload:
		return p.Content
	case map[string]any:
		if s, ok := p["content"].(string); ok {
			return s
		}
	}
	return ""
}

// isNotModified reports Telegram's "message is not modified" edit error, which
// is benign (identical snapshot).
func isNotModified(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Code == http.StatusBadRequest &&
		containsFold(ae.Description, "message is not modified")
}

func threadIDString(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
