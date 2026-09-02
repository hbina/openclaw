package channels

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tele "gopkg.in/telebot.v3"
)

type TelegramAdapter struct {
	bot         *tele.Bot
	ownerUserID string
}

func NewTelegramAdapter(token, ownerUserID string) (*TelegramAdapter, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	ownerID, err := strconv.ParseInt(ownerUserID, 10, 64)
	if err != nil || ownerID <= 0 {
		return nil, fmt.Errorf("telegram owner user id must be a positive integer")
	}
	pref := tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}

	b, err := tele.NewBot(pref)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize telegram bot: %w", err)
	}

	return &TelegramAdapter{bot: b, ownerUserID: ownerUserID}, nil
}

func (t *TelegramAdapter) ID() string {
	return "telegram"
}

func (t *TelegramAdapter) Start(ctx context.Context, handler Handler) error {
	t.bot.Handle(tele.OnText, func(c tele.Context) error {
		return handleTelegramText(ctx, c.Message(), t.bot.Me.ID, t.ownerUserID, handler)
	})

	go t.bot.Start()

	// Wait for context cancellation to stop polling
	go func() {
		<-ctx.Done()
		t.bot.Stop()
	}()

	return nil
}

func handleTelegramText(ctx context.Context, message *tele.Message, botID int64, ownerUserID string, handler Handler) error {
	msg, err := telegramInboundMessage(message, botID)
	if err != nil {
		return err
	}
	if msg.SenderID != ownerUserID {
		return nil
	}
	return handler(ctx, msg)
}

func telegramInboundMessage(message *tele.Message, botID int64) (*Message, error) {
	if message == nil {
		return nil, fmt.Errorf("telegram text update is missing a message")
	}
	if message.Sender == nil {
		return nil, fmt.Errorf("telegram text message is missing a sender")
	}

	inbound := &Message{
		ChannelID: "telegram",
		SenderID:  strconv.FormatInt(message.Sender.ID, 10),
		MessageID: strconv.Itoa(message.ID),
		Content:   message.Text,
	}
	if message.ReplyTo == nil {
		return inbound, nil
	}

	reply := message.ReplyTo
	author := ReplyAuthorOther
	if reply.Sender != nil {
		switch reply.Sender.ID {
		case botID:
			author = ReplyAuthorAssistant
		case message.Sender.ID:
			author = ReplyAuthorUser
		}
	}

	body := strings.TrimSpace(reply.Text)
	if body == "" {
		body = strings.TrimSpace(reply.Caption)
	}
	selectedText := ""
	if message.Quote != nil {
		selectedText = strings.TrimSpace(message.Quote.Text)
	}
	inbound.Reply = &ReplyContext{
		MessageID:          strconv.Itoa(reply.ID),
		Author:             author,
		Body:               body,
		SelectedText:       selectedText,
		ContentUnavailable: body == "",
	}
	return inbound, nil
}

func (t *TelegramAdapter) Stop(ctx context.Context) error {
	t.bot.Stop()
	return nil
}

func (t *TelegramAdapter) SendMessage(ctx context.Context, recipientID string, content string) (DeliveryReceipt, error) {
	id, err := strconv.ParseInt(recipientID, 10, 64)
	if err != nil {
		return DeliveryReceipt{}, fmt.Errorf("invalid recipient id format: %w", err)
	}

	user := &tele.User{ID: id}
	sent, err := t.bot.Send(user, content)
	if err != nil {
		return DeliveryReceipt{}, fmt.Errorf("failed to send telegram message: %w", err)
	}
	return DeliveryReceipt{MessageID: strconv.Itoa(sent.ID)}, nil
}
