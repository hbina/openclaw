package providers

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/openclaw/openclaw/go/internal/state"
)

type MCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type MCPResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   interface{} `json:"error,omitempty"`
}

func RunMCPServer(store *state.Store) {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Bytes()
		var req MCPRequest
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("MCP JSON parse error: %v", err)
			continue
		}

		if req.Method == "initialize" {
			sendMCPResponse(req.ID, map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]interface{}{
					"tools": map[string]interface{}{},
				},
				"serverInfo": map[string]interface{}{
					"name":    "openclaw-cron",
					"version": "1.0.0",
				},
			})
			continue
		}

		if req.Method == "tools/list" {
			sendMCPResponse(req.ID, map[string]interface{}{
				"tools": []map[string]interface{}{
					{
						"name":        "add_reminder",
						"description": "Schedule a reminder. The agent will notify the user when the time is reached.",
						"inputSchema": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"message": map[string]interface{}{
									"type":        "string",
									"description": "The message to remind the user about.",
								},
								"minutes": map[string]interface{}{
									"type":        "integer",
									"description": "How many minutes from now to fire the reminder.",
								},
								"channel_id": map[string]interface{}{
									"type":        "string",
									"description": "The current channel ID.",
								},
								"sender_id": map[string]interface{}{
									"type":        "string",
									"description": "The current user ID.",
								},
							},
							"required": []string{"message", "minutes", "channel_id", "sender_id"},
						},
					},
					{
						"name":        "list_reminders",
						"description": "List all currently scheduled reminders for the current user.",
						"inputSchema": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"channel_id": map[string]interface{}{
									"type": "string",
								},
								"sender_id": map[string]interface{}{
									"type": "string",
								},
							},
							"required": []string{"channel_id", "sender_id"},
						},
					},
				},
			})
			continue
		}

		if req.Method == "tools/call" {
			var params struct {
				Name      string                 `json:"name"`
				Arguments map[string]interface{} `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				sendMCPError(req.ID, -32602, "Invalid params")
				continue
			}

			if params.Name == "add_reminder" {
				msg, _ := params.Arguments["message"].(string)
				mins, _ := params.Arguments["minutes"].(float64)
				cID, _ := params.Arguments["channel_id"].(string)
				sID, _ := params.Arguments["sender_id"].(string)
				fireAt := time.Now().Add(time.Duration(mins) * time.Minute)

				err := store.AddReminder(cID, sID, msg, fireAt)
				if err != nil {
					sendMCPToolResult(req.ID, fmt.Sprintf("Error: %v", err), true)
				} else {
					sendMCPToolResult(req.ID, fmt.Sprintf("Reminder added successfully for %v", fireAt), false)
				}
				continue
			}

			if params.Name == "list_reminders" {
				cID, _ := params.Arguments["channel_id"].(string)
				sID, _ := params.Arguments["sender_id"].(string)
				reminders, err := store.ListReminders(cID, sID)
				if err != nil {
					sendMCPToolResult(req.ID, fmt.Sprintf("Error: %v", err), true)
					continue
				}
				if len(reminders) == 0 {
					sendMCPToolResult(req.ID, "No reminders currently scheduled.", false)
					continue
				}

				res := "Scheduled Reminders:\n"
				for _, r := range reminders {
					res += fmt.Sprintf("- ID: %d | Time: %v | Message: %s\n", r.ID, r.FireAt, r.Message)
				}
				sendMCPToolResult(req.ID, res, false)
				continue
			}

			sendMCPError(req.ID, -32601, "Tool not found")
			continue
		}

		// Ignore other notifications/methods (e.g., notifications/initialized)
	}
}

func sendMCPResponse(id interface{}, result interface{}) {
	if id == nil {
		return // Notification, no response needed
	}
	resp := MCPResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	b, _ := json.Marshal(resp)
	fmt.Println(string(b))
}

func sendMCPError(id interface{}, code int, message string) {
	if id == nil {
		return
	}
	resp := MCPResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: map[string]interface{}{
			"code":    code,
			"message": message,
		},
	}
	b, _ := json.Marshal(resp)
	fmt.Println(string(b))
}

func sendMCPToolResult(id interface{}, text string, isError bool) {
	sendMCPResponse(id, map[string]interface{}{
		"content": []map[string]interface{}{
			{
				"type": "text",
				"text": text,
			},
		},
		"isError": isError,
	})
}
