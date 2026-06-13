package channels

import (
	"context"
	"fmt"

	"github.com/bwmarrin/discordgo"
)

type DiscordAdapter struct {
	session *discordgo.Session
}

func NewDiscordAdapter(token string) (*DiscordAdapter, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("failed to create discord session: %w", err)
	}

	// We only need message intents for DMs and basic text messages
	s.Identify.Intents = discordgo.IntentsDirectMessages | discordgo.IntentsGuildMessages

	return &DiscordAdapter{session: s}, nil
}

func (d *DiscordAdapter) ID() string {
	return "discord"
}

func (d *DiscordAdapter) Start(ctx context.Context, handler Handler) error {
	d.session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		// Ignore all messages created by the bot itself
		if m.Author.ID == s.State.User.ID {
			return
		}

		msg := &Message{
			ChannelID: d.ID(),
			SenderID:  m.ChannelID, // Use channel ID to reply back to the same channel
			Content:   m.Content,
		}

		// Ignoring the error as handlers are running asynchronously
		_ = handler(ctx, msg)
	})

	if err := d.session.Open(); err != nil {
		return fmt.Errorf("failed to open discord connection: %w", err)
	}

	go func() {
		<-ctx.Done()
		d.session.Close()
	}()

	return nil
}

func (d *DiscordAdapter) Stop(ctx context.Context) error {
	return d.session.Close()
}

func (d *DiscordAdapter) SendMessage(ctx context.Context, recipientID string, content string) error {
	_, err := d.session.ChannelMessageSend(recipientID, content)
	if err != nil {
		return fmt.Errorf("failed to send discord message: %w", err)
	}
	return nil
}
