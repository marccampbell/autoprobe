package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/marccampbell/autoprobe/pkg/tools"
)

const apiURL = "https://api.anthropic.com/v1/messages"
const model = "claude-sonnet-4-20250514"

// Client is an Anthropic API client
type Client struct {
	apiKey     string
	httpClient *http.Client
}

// Message represents a conversation message
type Message struct {
	Role    string        `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock represents a block of content in a message
type ContentBlock struct {
	Type      string                 `json:"type"`
	Text      string                 `json:"text,omitempty"`
	ID        string                 `json:"id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Input     map[string]interface{} `json:"input,omitempty"`
	ToolUseID string                 `json:"tool_use_id,omitempty"`
	Content   string                 `json:"content,omitempty"`
	IsError   bool                   `json:"is_error,omitempty"`
}

// Request represents an API request
type Request struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	System    string         `json:"system,omitempty"`
	Messages  []Message      `json:"messages"`
	Tools     []tools.Tool   `json:"tools,omitempty"`
}

// Response represents an API response
type Response struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []ContentBlock `json:"content"`
	StopReason   string         `json:"stop_reason"`
	Usage        Usage          `json:"usage"`
}

// Usage represents token usage
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// NewClient creates a new Claude API client
func NewClient() (*Client, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not set. Get one at https://console.anthropic.com")
	}

	return &Client{
		apiKey:     apiKey,
		httpClient: &http.Client{},
	}, nil
}

// Complete sends a simple completion request without tools
func (c *Client) Complete(systemPrompt string, userPrompt string) (string, error) {
	messages := []Message{
		{
			Role: "user",
			Content: []ContentBlock{
				{Type: "text", Text: userPrompt},
			},
		},
	}

	resp, err := c.sendRequest(systemPrompt, messages, nil)
	if err != nil {
		return "", err
	}

	// Extract text from response
	var result string
	for _, block := range resp.Content {
		if block.Type == "text" {
			result += block.Text
		}
	}

	return result, nil
}

// RunWithTools executes a prompt with tools until completion
func (c *Client) RunWithTools(systemPrompt string, userPrompt string, availableTools []tools.Tool, onMessage func(string)) error {
	messages := []Message{
		{
			Role: "user",
			Content: []ContentBlock{
				{Type: "text", Text: userPrompt},
			},
		},
	}

	totalInputTokens := 0
	totalOutputTokens := 0
	maxTurns := 15 // Limit tool turns to prevent runaway context

	for turn := 0; turn < maxTurns; turn++ {
		resp, err := c.sendRequest(systemPrompt, messages, availableTools)
		if err != nil {
			return err
		}

		totalInputTokens += resp.Usage.InputTokens
		totalOutputTokens += resp.Usage.OutputTokens

		// Process response content
		var toolUses []tools.ToolUse
		for _, block := range resp.Content {
			if block.Type == "text" && block.Text != "" {
				if onMessage != nil {
					onMessage(block.Text)
				}
			} else if block.Type == "tool_use" {
				toolUses = append(toolUses, tools.ToolUse{
					ID:    block.ID,
					Name:  block.Name,
					Input: block.Input,
				})
			}
		}

		// If no tool uses, we're done
		if len(toolUses) == 0 || resp.StopReason == "end_turn" {
			return nil
		}

		// Add assistant's response to messages
		messages = append(messages, Message{
			Role:    "assistant",
			Content: resp.Content,
		})

		// Execute tools and collect results
		var toolResults []ContentBlock
		for _, tu := range toolUses {
			result := tools.ExecuteTool(tu)
			// Truncate large tool results to prevent context overflow
			content := result.Content
			if len(content) > 15000 {
				content = content[:15000] + "\n\n[TRUNCATED - content too large]"
			}
			toolResults = append(toolResults, ContentBlock{
				Type:      "tool_result",
				ToolUseID: result.ToolUseID,
				Content:   content,
				IsError:   result.IsError,
			})
			
			// Log tool use for verbose mode (via callback)
			if onMessage != nil {
				onMessage(fmt.Sprintf("\n[TOOL: %s]\n", tu.Name))
			}
		}

		// Add tool results to messages
		messages = append(messages, Message{
			Role:    "user",
			Content: toolResults,
		})
	}
	
	return nil // Max turns reached
}

func (c *Client) sendRequest(system string, messages []Message, availableTools []tools.Tool) (*Response, error) {
	req := Request{
		Model:     model,
		MaxTokens: 4096,
		System:    system,
		Messages:  messages,
		Tools:     availableTools,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if httpResp.StatusCode != 200 {
		return nil, fmt.Errorf("API error (%d): %s", httpResp.StatusCode, string(respBody))
	}

	var resp Response
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &resp, nil
}
