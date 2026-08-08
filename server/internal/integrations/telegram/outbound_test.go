package telegram

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type fakeTelegramOutboundQueries struct {
	task          db.AgentTaskQueue
	taskErr       error
	channelOrigin bool
	originErr     error
	binding       db.ChannelChatSessionBinding
	installation  db.ChannelInstallation
}

func (f *fakeTelegramOutboundQueries) GetAgentTask(context.Context, pgtype.UUID) (db.AgentTaskQueue, error) {
	return f.task, f.taskErr
}

func (f *fakeTelegramOutboundQueries) TaskHasChannelIngestedMessages(context.Context, pgtype.UUID) (bool, error) {
	return f.channelOrigin, f.originErr
}

func (f *fakeTelegramOutboundQueries) GetChannelChatSessionBindingBySession(context.Context, db.GetChannelChatSessionBindingBySessionParams) (db.ChannelChatSessionBinding, error) {
	return f.binding, nil
}

func (f *fakeTelegramOutboundQueries) GetChannelInstallation(context.Context, db.GetChannelInstallationParams) (db.ChannelInstallation, error) {
	return f.installation, nil
}

func telegramTestUUID(b byte) pgtype.UUID {
	var id pgtype.UUID
	id.Bytes[0] = b
	id.Valid = true
	return id
}

func telegramTestTask() db.AgentTaskQueue {
	return db.AgentTaskQueue{
		ChatSessionID:   telegramTestUUID(3),
		ChatInputTaskID: telegramTestUUID(4),
	}
}

func telegramTestEvent() events.Event {
	return events.Event{
		TaskID:        "00000000-0000-0000-0000-000000000002",
		ChatSessionID: "00000000-0000-0000-0000-000000000003",
		Type:          protocol.EventChatDone,
		Payload: protocol.ChatDonePayload{
			TaskID:        "00000000-0000-0000-0000-000000000002",
			ChatSessionID: "00000000-0000-0000-0000-000000000003",
			Content:       "reply",
		},
	}
}

func newTelegramOutboundQueries() *fakeTelegramOutboundQueries {
	return &fakeTelegramOutboundQueries{
		task: telegramTestTask(),
		binding: db.ChannelChatSessionBinding{
			InstallationID: telegramTestUUID(1),
			ChannelChatID:  "42",
			Config:         []byte(`{"chat_id":"42"}`),
		},
		installation: db.ChannelInstallation{
			ID:     telegramTestUUID(1),
			Status: "active",
			Config: []byte(`{"bot_token_encrypted":"MTIzOnNlY3JldA=="}`),
		},
	}
}

func TestResolveTargetFailsClosedWhenTaskLookupFails(t *testing.T) {
	q := newTelegramOutboundQueries()
	q.taskErr = errors.New("database unavailable")
	o := NewOutbound(q, nil, "", nil, nil)

	if _, err := o.resolveTarget(context.Background(), telegramTestEvent(), false); err == nil {
		t.Fatal("expected task lookup error")
	}
}

func TestResolveTargetSkipsDirectTaskOnBoundTelegramSession(t *testing.T) {
	q := newTelegramOutboundQueries()
	o := NewOutbound(q, nil, "", nil, nil)

	target, err := o.resolveTarget(context.Background(), telegramTestEvent(), false)
	if err != nil {
		t.Fatal(err)
	}
	if target != nil {
		t.Fatalf("target = %+v, want nil for a direct task", target)
	}
}

func TestResolveTargetDeliversChannelTaskReply(t *testing.T) {
	q := newTelegramOutboundQueries()
	q.channelOrigin = true
	o := NewOutbound(q, nil, "", nil, nil)

	target, err := o.resolveTarget(context.Background(), telegramTestEvent(), false)
	if err != nil {
		t.Fatal(err)
	}
	if target == nil || target.chatID != 42 || target.botToken != "123:secret" {
		t.Fatalf("target = %+v", target)
	}
}

func TestValidateBindingTokenChannel(t *testing.T) {
	if err := validateBindingTokenChannel(db.ChannelBindingToken{ChannelType: string(TypeTelegram)}); err != nil {
		t.Fatalf("telegram token rejected: %v", err)
	}
	if err := validateBindingTokenChannel(db.ChannelBindingToken{ChannelType: "slack"}); !errors.Is(err, ErrBindingTokenInvalid) {
		t.Fatalf("foreign token error = %v, want ErrBindingTokenInvalid", err)
	}
}
