package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This file is the Telegram OutboundReplier — the engine seam that delivers a
// verdict-driven reply back to the user, mirroring slack/replier.go:
//   - NeedsBinding: mint a single-use binding token, reply with the link to
//     the in-product redeem page (/telegram/bind).
//   - AgentOffline / AgentArchived: a status notice.
//   - Ingested with an /issue created: a confirmation.

// bindingMinter is the binding-token surface the replier needs.
// *BindingTokenService satisfies it.
type bindingMinter interface {
	Mint(ctx context.Context, workspaceID, installationID pgtype.UUID, telegramUserID string) (BindingToken, error)
}

// OutboundReplier implements engine.OutboundReplier for Telegram.
type OutboundReplier struct {
	binding     bindingMinter
	decrypt     Decrypter
	appURL      string
	bindingPath string
	apiBase     string
	client      *http.Client
	logger      *slog.Logger
}

// OutboundReplierConfig configures the replier. Binding + AppURL are required
// for the NeedsBinding prompt; without them the prompt is skipped (other
// notices still fire).
type OutboundReplierConfig struct {
	Binding bindingMinter
	Decrypt Decrypter
	// AppURL is the Multica web app host for the redeem link, same sourcing as
	// the Slack replier (MULTICA_APP_URL ?? FRONTEND_ORIGIN).
	AppURL      string
	BindingPath string // default "/telegram/bind"
	APIBase     string
	HTTPClient  *http.Client
	Logger      *slog.Logger
}

var _ engine.OutboundReplier = (*OutboundReplier)(nil)

// NewOutboundReplier builds the replier.
func NewOutboundReplier(cfg OutboundReplierConfig) *OutboundReplier {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	bindingPath := cfg.BindingPath
	if bindingPath == "" {
		bindingPath = "/telegram/bind"
	}
	if !strings.HasPrefix(bindingPath, "/") {
		bindingPath = "/" + bindingPath
	}
	return &OutboundReplier{
		binding:     cfg.Binding,
		decrypt:     cfg.Decrypt,
		appURL:      strings.TrimRight(cfg.AppURL, "/"),
		bindingPath: bindingPath,
		apiBase:     cfg.APIBase,
		client:      cfg.HTTPClient,
		logger:      logger,
	}
}

// Reply routes each outcome to its user-visible message. Errors are logged,
// not propagated: the replier runs detached from the inbound ACK path.
func (r *OutboundReplier) Reply(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) {
	switch res.Outcome {
	case engine.OutcomeNeedsBinding:
		if err := r.sendBindingPrompt(ctx, inst, msg, res); err != nil {
			r.logger.WarnContext(ctx, "telegram replier: binding prompt failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeAgentOffline:
		if err := r.post(ctx, inst, msg, msgAgentOffline); err != nil {
			r.logger.WarnContext(ctx, "telegram replier: offline notice failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeAgentArchived:
		if err := r.post(ctx, inst, msg, msgAgentArchived); err != nil {
			r.logger.WarnContext(ctx, "telegram replier: archived notice failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeIngested:
		if res.IssueID.Valid {
			if err := r.post(ctx, inst, msg, issueCreatedText(res)); err != nil {
				r.logger.WarnContext(ctx, "telegram replier: issue confirmation failed",
					"installation_id", util.UUIDToString(inst.ID), "error", err)
			}
		}
	}
}

func (r *OutboundReplier) sendBindingPrompt(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) error {
	sender := res.Sender
	if sender == "" {
		sender = msg.Source.SenderID
	}
	if sender == "" {
		return errors.New("missing sender id")
	}
	if r.binding == nil {
		return errors.New("binding service not configured")
	}
	if r.appURL == "" {
		return errors.New("app url not configured")
	}
	token, err := r.binding.Mint(ctx, inst.WorkspaceID, inst.ID, sender)
	if err != nil {
		return fmt.Errorf("mint binding token: %w", err)
	}
	bindURL := r.appURL + r.bindingPath + "?token=" + url.QueryEscape(token.Raw)
	text := "👋 要开始和我对话，请先绑定你的 Multica 账号：\n" + bindURL + "\n（链接 15 分钟内有效）"
	return r.post(ctx, inst, msg, text)
}

// post resolves the installation's bot token from the carried platform row
// and sends plain text back into the originating chat / topic.
func (r *OutboundReplier) post(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, text string) error {
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		return errors.New("installation platform row unavailable")
	}
	creds, err := decodeCredentials(row.Config, r.decrypt)
	if err != nil {
		return fmt.Errorf("decode credentials: %w", err)
	}
	chatID, err := strconv.ParseInt(msg.Source.ChatID, 10, 64)
	if err != nil {
		return fmt.Errorf("bad chat id %q: %w", msg.Source.ChatID, err)
	}
	var threadID int64
	if msg.Source.ThreadID != "" {
		threadID, _ = strconv.ParseInt(msg.Source.ThreadID, 10, 64)
	}
	if _, err := newBotAPI(r.apiBase, creds.BotToken, r.client).SendMessage(ctx, sendMessageParams{
		ChatID:          chatID,
		Text:            text,
		MessageThreadID: threadID,
	}); err != nil {
		return fmt.Errorf("post telegram reply: %w", err)
	}
	return nil
}

func issueCreatedText(res engine.Result) string {
	id := res.IssueIdentifier
	if id == "" {
		id = fmt.Sprintf("#%d", res.IssueNumber)
	}
	title := strings.TrimSpace(res.IssueTitle)
	if title == "" {
		return "✅ 已创建 " + id
	}
	return "✅ 已创建 " + id + " — " + title
}
