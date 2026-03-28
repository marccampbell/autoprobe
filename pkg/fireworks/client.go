package fireworks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/marccampbell/autoprobe/pkg/tools"
)

const apiURL = "https://api.fireworks.ai/inference/v1/chat/completions"
const defaultModel = "accounts/fireworks/models/kimi-k2p5"

// Client is a Fireworks API client
type Client struct {
	apiKey     string
	model      string
	httpClient *http.Client
}

// Message represents a conversation message
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall represents a tool call from the model
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall represents the function being called
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Request represents an API request
type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`
}

// Tool represents a tool definition for the API
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction represents the function definition
type ToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// Response represents an API response
type Response struct {
	ID      string   `json:"id"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice represents a response choice
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage represents token usage
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// NewClient creates a new Fireworks API client
func NewClient() (*Client, error) {
	apiKey := os.Getenv("FIREWORKS_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("FIREWORKS_API_KEY not set")
	}

	return &Client{
		apiKey:     apiKey,
		model:      defaultModel,
		httpClient: &http.Client{},
	}, nil
}

// convertTools converts our internal tool format to Fireworks format
func convertTools(internalTools []tools.Tool) []Tool {
	result := make([]Tool, len(internalTools))
	for i, t := range internalTools {
		result[i] = Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		}
	}
	return result
}

// RunWithTools executes a prompt with tools until completion
func (c *Client) RunWithTools(systemPrompt string, userPrompt string, availableTools []tools.Tool, onMessage func(string), onToolUse func(string)) error {
	messages := []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}

	fwTools := convertTools(availableTools)
	maxTurns := 30

	for turn := 0; turn < maxTurns; turn++ {
		resp, err := c.sendRequest(messages, fwTools)
		if err != nil {
			return err
		}

		if len(resp.Choices) == 0 {
			return fmt.Errorf("no choices in response")
		}

		choice := resp.Choices[0]
		assistantMsg := choice.Message

		// Handle text content
		if assistantMsg.Content != "" && onMessage != nil {
			onMessage(assistantMsg.Content)
		}

		// If no tool calls, we're done
		if len(assistantMsg.ToolCalls) == 0 || choice.FinishReason == "stop" {
			return nil
		}

		// Add assistant message to history
		messages = append(messages, assistantMsg)

		// Execute tool calls
		for _, tc := range assistantMsg.ToolCalls {
			if onToolUse != nil {
				onToolUse(tc.Function.Name)
			}

			// Parse arguments
			var args map[string]interface{}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				args = make(map[string]interface{})
			}

			// Execute tool
			result := tools.ExecuteTool(tools.ToolUse{
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: args,
			})

			// Truncate large results
			content := result.Content
			if len(content) > 15000 {
				content = content[:15000] + "\n\n[TRUNCATED]"
			}

			// Add tool result
			messages = append(messages, Message{
				Role:       "tool",
				Content:    content,
				ToolCallID: tc.ID,
			})
		}
	}

	return nil
}

func (c *Client) sendRequest(messages []Message, tools []Tool) (*Response, error) {
	req := Request{
		Model:    c.model,
		Messages: messages,
		Tools:    tools,
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
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

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
