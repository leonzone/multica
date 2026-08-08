package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

func TestParseBotID(t *testing.T) {
	cases := []struct {
		token  string
		want   string
		wantOK bool
	}{
		{"8983760937:AAExampleSecretPart", "8983760937", true},
		{" 12345:abc ", "12345", true},
		{"no-colon-token", "", false},
		{":empty-id", "", false},
		{"abc123:secret", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, err := parseBotID(c.token)
		if c.wantOK && (err != nil || got != c.want) {
			t.Errorf("parseBotID(%q) = %q, %v; want %q", c.token, got, err, c.want)
		}
		if !c.wantOK && err == nil {
			t.Errorf("parseBotID(%q) should fail", c.token)
		}
	}
}

func TestInboundFromUpdatePrivateText(t *testing.T) {
	u := Update{
		UpdateID: 42,
		Message: &Message{
			MessageID: 7,
			From:      &User{ID: 111, FirstName: "Ada", LastName: "L"},
			Chat:      Chat{ID: 555, Type: "private"},
			Text:      "hello there",
		},
	}
	msg, ok := inboundFromUpdate(u, 999, "my_bot")
	if !ok {
		t.Fatal("expected ok")
	}
	if msg.EventID != "42" || msg.MessageID != "555:7" {
		t.Errorf("ids: event=%q message=%q", msg.EventID, msg.MessageID)
	}
	if msg.Source.ChatType != channel.ChatTypeP2P || !msg.AddressedToBot {
		t.Errorf("p2p should always be addressed: %+v", msg.Source)
	}
	if msg.Text != "hello there" || msg.Type != channel.MsgTypeText {
		t.Errorf("text=%q type=%q", msg.Text, msg.Type)
	}
	var raw telegramRawEvent
	if err := json.Unmarshal(msg.Raw, &raw); err != nil || raw.BotID != "999" || raw.SenderName != "Ada L" {
		t.Errorf("raw = %+v, err %v", raw, err)
	}
}

func TestInboundFromUpdateGroupAddressing(t *testing.T) {
	base := func(text string, reply *Message) Update {
		return Update{
			UpdateID: 1,
			Message: &Message{
				MessageID:      2,
				From:           &User{ID: 111, FirstName: "U"},
				Chat:           Chat{ID: -100200, Type: "supergroup"},
				Text:           text,
				ReplyToMessage: reply,
			},
		}
	}
	// Unaddressed group chatter: ingested but not addressed (Router drops it).
	msg, ok := inboundFromUpdate(base("plain chatter", nil), 999, "my_bot")
	if !ok || msg.AddressedToBot {
		t.Errorf("plain group message must not be addressed: ok=%v addressed=%v", ok, msg.AddressedToBot)
	}
	// @-mention: addressed, mention token stripped.
	msg, ok = inboundFromUpdate(base("@my_bot do the thing", nil), 999, "my_bot")
	if !ok || !msg.AddressedToBot || msg.Text != "do the thing" {
		t.Errorf("mention: ok=%v addressed=%v text=%q", ok, msg.AddressedToBot, msg.Text)
	}
	// Reply to the bot's own message: addressed.
	botMsg := &Message{MessageID: 1, From: &User{ID: 999, IsBot: true}}
	msg, ok = inboundFromUpdate(base("follow-up", botMsg), 999, "my_bot")
	if !ok || !msg.AddressedToBot {
		t.Errorf("reply-to-bot must be addressed: ok=%v addressed=%v", ok, msg.AddressedToBot)
	}
}

func TestInboundFromUpdateDropsBotsAndChannels(t *testing.T) {
	if _, ok := inboundFromUpdate(Update{Message: &Message{
		From: &User{ID: 5, IsBot: true}, Chat: Chat{ID: 1, Type: "private"}, Text: "x",
	}}, 999, "b"); ok {
		t.Error("bot sender must be dropped")
	}
	if _, ok := inboundFromUpdate(Update{Message: &Message{
		From: &User{ID: 5}, Chat: Chat{ID: 1, Type: "channel"}, Text: "x",
	}}, 999, "b"); ok {
		t.Error("channel post must be dropped")
	}
	if _, ok := inboundFromUpdate(Update{}, 999, "b"); ok {
		t.Error("empty update must be dropped")
	}
}

func TestInboundFreshCommand(t *testing.T) {
	u := Update{UpdateID: 1, Message: &Message{
		MessageID: 2, From: &User{ID: 3, FirstName: "U"},
		Chat: Chat{ID: 4, Type: "private"}, Text: "/fresh start over please",
	}}
	msg, ok := inboundFromUpdate(u, 999, "my_bot")
	if !ok || !msg.ForceFresh || msg.Text != "start over please" {
		t.Errorf("fresh: ok=%v force=%v text=%q", ok, msg.ForceFresh, msg.Text)
	}
}

func TestTelegramSessionRouting(t *testing.T) {
	p2p := channel.InboundMessage{Source: channel.Source{
		ChatID: "555", ChatType: channel.ChatTypeP2P,
	}}
	key, cfg, thread := telegramSessionRouting(p2p)
	if key != "555" || thread != "" {
		t.Errorf("p2p: key=%q thread=%q", key, thread)
	}
	var bc telegramBindingConfig
	if err := json.Unmarshal(cfg, &bc); err != nil || bc.ChatID != "555" {
		t.Errorf("binding config = %+v, err %v", bc, err)
	}
	topic := channel.InboundMessage{Source: channel.Source{
		ChatID: "-100", ChatType: channel.ChatTypeGroup, ThreadID: "77",
	}}
	key, _, thread = telegramSessionRouting(topic)
	if key != "-100:77" || thread != "77" {
		t.Errorf("forum topic: key=%q thread=%q", key, thread)
	}
}

func TestGetUpdates409IsErrConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request"}`))
	}))
	defer srv.Close()
	api := newBotAPI(srv.URL, "123:abc", srv.Client())
	if _, err := api.GetUpdates(context.Background(), 0); err != ErrConflict {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestRetryAfterOn429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":7}}`))
	}))
	defer srv.Close()
	api := newBotAPI(srv.URL, "123:abc", srv.Client())
	_, err := api.SendMessage(context.Background(), sendMessageParams{ChatID: 1, Text: "x"})
	wait, ok := retryAfter(err)
	if !ok || wait != 7*time.Second {
		t.Fatalf("retryAfter = %v, %v; want 7s", wait, ok)
	}
}

func TestGetWebhookInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/getWebhookInfo") {
			t.Fatalf("path = %q, want getWebhookInfo", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"url":"https://example.test/telegram","pending_update_count":3}}`))
	}))
	defer srv.Close()

	info, err := newBotAPI(srv.URL, "123:secret", srv.Client()).GetWebhookInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.URL != "https://example.test/telegram" || info.PendingUpdateCount != 3 {
		t.Fatalf("webhook info = %+v", info)
	}
}

func TestTransportErrorDoesNotExposeBotToken(t *testing.T) {
	transportErr := errors.New("dial tcp 123:secret@example.test:443: connection refused")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, transportErr
	})}

	_, err := newBotAPI("https://api.example.test", "123:secret", client).GetMe(context.Background())
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), "123:secret") {
		t.Fatalf("transport error exposed bot token: %v", err)
	}
	if !errors.Is(err, transportErr) {
		t.Fatalf("errors.Is lost transport cause: %v", err)
	}
}

func TestConnectDispatchesAndAdvancesOffset(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "getUpdates") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
			return
		}
		n := calls.Add(1)
		var body getUpdatesParams
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch n {
		case 1:
			if body.Offset != 0 {
				t.Errorf("first poll offset = %d, want 0", body.Offset)
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":10,"message":{"message_id":1,"from":{"id":42,"first_name":"A"},"chat":{"id":42,"type":"private"},"text":"hi"}}]}`))
		case 2:
			if body.Offset != 11 {
				t.Errorf("second poll offset = %d, want 11", body.Offset)
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
		default:
			// Park until the test cancels.
			<-r.Context().Done()
		}
	}))
	defer srv.Close()

	var received []channel.InboundMessage
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := &telegramChannel{
		botID:       999,
		botUsername: "my_bot",
		api:         newBotAPI(srv.URL, "123:abc", srv.Client()),
		handler: func(ctx context.Context, m channel.InboundMessage) error {
			received = append(received, m)
			close(done)
			return nil
		},
		logger: testLogger(),
	}
	go func() { _ = ch.Connect(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("message not dispatched")
	}
	cancel()
	if len(received) != 1 || received[0].Text != "hi" {
		t.Fatalf("received = %+v", received)
	}
}

func TestChunkMessagePrefersNewlines(t *testing.T) {
	text := strings.Repeat("a", 60) + "\n" + strings.Repeat("b", 60)
	chunks := chunkMessage(text, 100)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d: %v", len(chunks), chunks)
	}
	if !strings.HasSuffix(chunks[0], "a") || !strings.HasPrefix(chunks[1], "b") {
		t.Errorf("split should land on the newline: %q | %q", chunks[0], chunks[1])
	}
}

func TestChunkMessageCountsUTF16Units(t *testing.T) {
	chunks := chunkMessage("😀a😀", 3)
	if len(chunks) != 2 || chunks[0] != "😀a" || chunks[1] != "😀" {
		t.Fatalf("chunks = %#v", chunks)
	}
	for _, chunk := range chunks {
		if got := utf16Units(chunk); got > 3 {
			t.Errorf("chunk %q uses %d UTF-16 units", chunk, got)
		}
	}
}

func TestSenderFallsBackOnlyForHTMLParseErrors(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7,"chat":{"id":42,"type":"private"}}}`))
	}))
	defer srv.Close()

	s := newSender(newBotAPI(srv.URL, "123:secret", srv.Client()), testLogger())
	result, err := s.Send(context.Background(), channel.OutboundMessage{ChatID: "42", Text: "literal <tag>"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result.MessageID != "42:7" {
		t.Fatalf("calls=%d result=%+v", calls, result)
	}
}

func TestFormatHTML(t *testing.T) {
	got := formatHTML("# Title\n**bold** and `code` and [link](https://e.co/a_b)\n```go\nx < 1\n```")
	for _, want := range []string{
		"<b>Title</b>",
		"<b>bold</b>",
		"<code>code</code>",
		`<a href="https://e.co/a_b">link</a>`,
		`<pre><code class="language-go">x &lt; 1</code></pre>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatHTML missing %q in:\n%s", want, got)
		}
	}
	if plain := formatHTML("a < b & c"); !strings.Contains(plain, "a &lt; b &amp; c") {
		t.Errorf("entities must be escaped: %q", plain)
	}
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
