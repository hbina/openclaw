package channels

import (
	"context"
	"fmt"
	"strconv"
	"time"

	tele "gopkg.in/telebot.v3"
)

type TelegramAdapter struct {
	bot *tele.Bot
}

func NewTelegramAdapter(token string) (*TelegramAdapter, error) {
	pref := tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}

	b, err := tele.NewBot(pref)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize telegram bot: %w", err)
	}

	return &TelegramAdapter{bot: b}, nil
}

func (t *TelegramAdapter) ID() string {
	return "telegram"
}

func (t *TelegramAdapter) Start(ctx context.Context, handler Handler) error {
	t.bot.Handle(tele.OnText, func(c tele.Context) error {
		msg := &Message{
			ChannelID: t.ID(),
			SenderID:  strconv.FormatInt(c.Sender().ID, 10),
			Content:   c.Text(),
		}
		if err := handler(ctx, msg); err != nil {
			return err
		}
		return nil
	})

	go t.bot.Start()

	// Wait for context cancellation to stop polling
	go func() {
		<-ctx.Done()
		t.bot.Stop()
	}()

	return nil
}

func (t *TelegramAdapter) Stop(ctx context.Context) error {
	t.bot.Stop()
	return nil
}

func (t *TelegramAdapter) SendMessage(ctx context.Context, recipientID string, content string) error {
	id, err := strconv.ParseInt(recipientID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid recipient id format: %w", err)
	}

	user := &tele.User{ID: id}
	_, err = t.bot.Send(user, content)
	if err != nil {
		return fmt.Errorf("failed to send telegram message: %w", err)
	}

	return nil
}
