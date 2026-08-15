package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/openclaw/go/internal/state"
)

func TestTraceCommandsReadCanonicalDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := state.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.StartResponseTrace(context.Background(), state.TraceInput{TriggerType: "chat", ChannelID: "cli", SenderID: "owner", InputJSON: `{"content":"hello"}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishTrace(context.Background(), id, "failed", "model", context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var list bytes.Buffer
	if err := runTraceList([]string{"--database", path, "--json"}, &list); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list.String(), `"sender_id": "owner"`) {
		t.Fatalf("list=%s", list.String())
	}
	var show bytes.Buffer
	if err := runTraceShow([]string{"--database", path, "--id", "1"}, &show); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(show.String(), "Trace 1 (failed)") {
		t.Fatalf("show=%s", show.String())
	}
}
