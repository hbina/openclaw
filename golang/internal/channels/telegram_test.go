package channels

import (
	"testing"

	tele "gopkg.in/telebot.v3"
)

func TestTelegramInboundMessageWithoutReply(t *testing.T) {
	message := &tele.Message{
		ID:     10,
		Sender: &tele.User{ID: 100},
		Text:   "hello",
	}

	inbound, err := telegramInboundMessage(message, 200)
	if err != nil {
		t.Fatalf("telegramInboundMessage: %v", err)
	}
	if inbound.ChannelID != "telegram" || inbound.SenderID != "100" || inbound.Content != "hello" {
		t.Fatalf("unexpected inbound message: %#v", inbound)
	}
	if inbound.Reply != nil {
		t.Fatalf("ordinary message has reply context: %#v", inbound.Reply)
	}
}

func TestTelegramInboundMessageExtractsAssistantReplyAndSelectedText(t *testing.T) {
	message := &tele.Message{
		ID:     11,
		Sender: &tele.User{ID: 100},
		Text:   "Can you move that to 4?",
		ReplyTo: &tele.Message{
			ID:     9,
			Sender: &tele.User{ID: 200},
			Text:   "The appointment is at 3 PM.",
		},
		Quote: &tele.TextQuote{Text: "3 PM", Manual: true},
	}

	inbound, err := telegramInboundMessage(message, 200)
	if err != nil {
		t.Fatalf("telegramInboundMessage: %v", err)
	}
	want := &ReplyContext{
		MessageID:    "9",
		Author:       ReplyAuthorAssistant,
		Body:         "The appointment is at 3 PM.",
		SelectedText: "3 PM",
	}
	if inbound.Reply == nil || *inbound.Reply != *want {
		t.Fatalf("reply context = %#v, want %#v", inbound.Reply, want)
	}
}

func TestTelegramInboundMessageUsesCaptionAndClassifiesOwner(t *testing.T) {
	message := &tele.Message{
		ID:     12,
		Sender: &tele.User{ID: 100},
		Text:   "Use this one",
		ReplyTo: &tele.Message{
			ID:      8,
			Sender:  &tele.User{ID: 100},
			Caption: "  receipt caption  ",
		},
	}

	inbound, err := telegramInboundMessage(message, 200)
	if err != nil {
		t.Fatalf("telegramInboundMessage: %v", err)
	}
	if inbound.Reply == nil || inbound.Reply.Author != ReplyAuthorUser ||
		inbound.Reply.Body != "receipt caption" || inbound.Reply.ContentUnavailable {
		t.Fatalf("unexpected reply context: %#v", inbound.Reply)
	}
}

func TestTelegramInboundMessageMarksUnavailableOtherContent(t *testing.T) {
	message := &tele.Message{
		ID:     13,
		Sender: &tele.User{ID: 100},
		Text:   "What is this?",
		ReplyTo: &tele.Message{
			ID:     7,
			Sender: &tele.User{ID: 300},
			Photo:  &tele.Photo{},
		},
	}

	inbound, err := telegramInboundMessage(message, 200)
	if err != nil {
		t.Fatalf("telegramInboundMessage: %v", err)
	}
	if inbound.Reply == nil || inbound.Reply.Author != ReplyAuthorOther ||
		inbound.Reply.Body != "" || !inbound.Reply.ContentUnavailable {
		t.Fatalf("unexpected reply context: %#v", inbound.Reply)
	}
}

func TestTelegramInboundMessageRejectsMissingSender(t *testing.T) {
	if _, err := telegramInboundMessage(nil, 200); err == nil {
		t.Fatal("nil message was accepted")
	}
	if _, err := telegramInboundMessage(&tele.Message{Text: "hello"}, 200); err == nil {
		t.Fatal("message without sender was accepted")
	}
}
