package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type fakeTelegramBindingMinter struct {
	calls int
	raw   string
}

func (f *fakeTelegramBindingMinter) Mint(context.Context, pgtype.UUID, pgtype.UUID, string) (BindingToken, error) {
	f.calls++
	return BindingToken{Raw: f.raw}, nil
}

func TestReplyNeedsBindingDoesNotMintBearerLinkInGroup(t *testing.T) {
	var got sendMessageParams
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"chat":{"id":42,"type":"group"}}}`))
	}))
	defer srv.Close()

	minter := &fakeTelegramBindingMinter{raw: "secret-token"}
	r := NewOutboundReplier(OutboundReplierConfig{
		Binding:    minter,
		Decrypt:    nil,
		AppURL:     "https://multica.example",
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
		Logger:     testLogger(),
	})
	inst := engine.ResolvedInstallation{
		ID:          telegramTestUUID(1),
		WorkspaceID: telegramTestUUID(2),
		Platform:    db.ChannelInstallation{Config: []byte(`{"bot_token_encrypted":"MTIzOnNlY3JldA=="}`)},
	}
	msg := channel.InboundMessage{Source: channel.Source{
		ChatID: "42", ChatType: channel.ChatTypeGroup, SenderID: "telegram-user",
	}}

	r.Reply(context.Background(), inst, msg, engine.Result{
		Outcome: engine.OutcomeNeedsBinding,
		Sender:  "telegram-user",
	})

	if minter.calls != 0 {
		t.Fatalf("Mint called %d times for a group prompt", minter.calls)
	}
	if !strings.Contains(got.Text, msgBindingGroupHint) {
		t.Fatalf("group prompt = %q, want %q", got.Text, msgBindingGroupHint)
	}
	if strings.Contains(got.Text, "secret-token") || strings.Contains(got.Text, "multica.example") {
		t.Fatalf("group prompt exposed a redeem link: %q", got.Text)
	}
}
