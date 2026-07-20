package channels

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"go.mau.fi/whatsmeow/proto/waE2E"
	_ "github.com/mattn/go-sqlite3"
)

type WhatsAppAdapter struct {
	client *whatsmeow.Client
}

func NewWhatsAppAdapter(ctx context.Context, dbPath string) (*WhatsAppAdapter, error) {
	dbLog := waLog.Stdout("Database", "WARN", true)
	container, err := sqlstore.New(ctx, "sqlite3", fmt.Sprintf("file:%s?_foreign_keys=on", dbPath), dbLog)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to whatsmeow database: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get device store: %w", err)
	}

	clientLog := waLog.Stdout("Client", "WARN", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	return &WhatsAppAdapter{client: client}, nil
}

func (w *WhatsAppAdapter) ID() string {
	return "whatsapp"
}

func (w *WhatsAppAdapter) Start(ctx context.Context, handler Handler) error {
	w.client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			if v.Info.IsFromMe {
				return
			}
			msg := &Message{
				ChannelID: w.ID(),
				SenderID:  v.Info.Chat.String(),
				Content:   v.Message.GetConversation(),
			}
			// Only handle text messages for now
			if msg.Content != "" {
				_ = handler(ctx, msg)
			}
		}
	})

	if w.client.Store.ID == nil {
		// No ID stored, new login needed. In a real app we'd expose the QR code.
		// For the clean architecture skeleton, we just log a message.
		fmt.Println("WhatsApp login required: QR Code handling to be implemented.")
	} else {
		if err := w.client.Connect(); err != nil {
			return fmt.Errorf("failed to connect whatsapp client: %w", err)
		}
	}

	go func() {
		<-ctx.Done()
		w.client.Disconnect()
	}()

	return nil
}

func (w *WhatsAppAdapter) Stop(ctx context.Context) error {
	w.client.Disconnect()
	return nil
}

func (w *WhatsAppAdapter) SendMessage(ctx context.Context, recipientID string, content string) error {
	jid, err := types.ParseJID(recipientID)
	if err != nil {
		return fmt.Errorf("invalid recipient JID format: %w", err)
	}

	msg := &waE2E.Message{
		Conversation: &content,
	}

	_, err = w.client.SendMessage(ctx, jid, msg)
	if err != nil {
		return fmt.Errorf("failed to send whatsapp message: %w", err)
	}

	return nil
}
