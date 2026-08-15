package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/openclaw/openclaw/go/internal/state"
)

func runTraceCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: openclaw trace list|show [options]")
	}
	switch args[0] {
	case "list":
		return runTraceList(args[1:], os.Stdout)
	case "show":
		return runTraceShow(args[1:], os.Stdout)
	default:
		return fmt.Errorf("unknown trace command %q", args[0])
	}
}

func runTraceList(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("trace list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	database := flags.String("database", "", "SQLite database path")
	limit := flags.Int("limit", 20, "maximum rows")
	channel := flags.String("channel", "", "channel filter")
	sender := flags.String("sender", "", "sender filter")
	status := flags.String("status", "", "status filter")
	messageID := flags.String("message-id", "", "inbound or provider message id")
	sinceRaw := flags.String("since", "", "RFC3339 lower time bound")
	asJSON := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *database == "" {
		return fmt.Errorf("--database is required")
	}
	if *limit < 1 || *limit > 1000 {
		return fmt.Errorf("--limit must be between 1 and 1000")
	}
	var since *time.Time
	if *sinceRaw != "" {
		parsed, err := time.Parse(time.RFC3339, *sinceRaw)
		if err != nil {
			return fmt.Errorf("invalid --since: %w", err)
		}
		since = &parsed
	}
	store, err := state.OpenReadOnlyStore(*database)
	if err != nil {
		return err
	}
	defer store.Close()
	items, err := store.ListResponseTraces(context.Background(), state.TraceFilter{Limit: *limit, ChannelID: *channel, SenderID: *sender, Status: *status, ExternalMessageID: *messageID, Since: since})
	if err != nil {
		return err
	}
	if *asJSON {
		encoded, err := json.MarshalIndent(items, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, string(encoded))
		return err
	}
	w := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TRACE\tTIME\tTRIGGER\tROUTE\tSTATUS\tRESPONSE")
	for _, item := range items {
		preview := strings.Join(strings.Fields(item.FinalContent), " ")
		if len(preview) > 80 {
			preview = preview[:77] + "..."
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s/%s\t%s\t%s\n", item.ID, item.StartedAt.Format(time.RFC3339), item.TriggerType, item.ChannelID, item.SenderID, item.Status, preview)
	}
	return w.Flush()
}

func runTraceShow(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("trace show", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	database := flags.String("database", "", "SQLite database path")
	id := flags.Int64("id", 0, "trace id")
	asJSON := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *database == "" {
		return fmt.Errorf("--database is required")
	}
	if *id < 1 {
		return fmt.Errorf("--id must be positive")
	}
	store, err := state.OpenReadOnlyStore(*database)
	if err != nil {
		return err
	}
	defer store.Close()
	report, err := store.GetTraceReport(context.Background(), *id)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if *asJSON {
		_, err = fmt.Fprintln(output, string(encoded))
		return err
	}
	fmt.Fprintf(output, "Trace %s (%v)\n", strconv.FormatInt(*id, 10), report.Trace["status"])
	fmt.Fprintf(output, "Route: %v/%v\nTrigger: %v\nInput: %v\n\n", report.Trace["channel_id"], report.Trace["sender_id"], report.Trace["trigger_type"], report.Trace["input"])
	for _, event := range report.Events {
		detail, _ := json.MarshalIndent(event["detail"], "  ", "  ")
		fmt.Fprintf(output, "%v. %v [%v]\n%s\n", event["sequence_no"], event["kind"], event["status"], detail)
	}
	return nil
}
