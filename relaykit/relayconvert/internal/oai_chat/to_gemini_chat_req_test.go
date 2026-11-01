package oaichat

import (
	"context"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAIChatRequestToGeminiOmitsTrailingAssistantText(t *testing.T) {
	got, err := OpenAIChatRequestToGeminiGenerateContent(context.Background(), dto.GeneralOpenAIRequest{
		Model: "gemini-4-argon",
		Messages: []dto.Message{
			{Role: "system", Content: "You are a coding assistant."},
			{Role: "user", Content: "Say hello."},
			{Role: "assistant", Content: "Hello."},
		},
	}, nil)
	require.NoError(t, err)
	require.Len(t, got.Contents, 1)
	assert.Equal(t, "user", got.Contents[0].Role)
	assert.Equal(t, "Say hello.", got.Contents[0].Parts[0].Text)
	require.NotNil(t, got.SystemInstructions)
}

func TestOpenAIChatRequestToGeminiKeepsAssistantHistoryBeforeTheLastUserTurn(t *testing.T) {
	got, err := OpenAIChatRequestToGeminiGenerateContent(context.Background(), dto.GeneralOpenAIRequest{
		Model: "gemini-4-argon",
		Messages: []dto.Message{
			{Role: "user", Content: "First"},
			{Role: "assistant", Content: "Reply"},
			{Role: "user", Content: "Second"},
		},
	}, nil)
	require.NoError(t, err)
	require.Len(t, got.Contents, 3)
	assert.Equal(t, []string{"user", "model", "user"}, []string{
		got.Contents[0].Role, got.Contents[1].Role, got.Contents[2].Role,
	})
}

func TestOpenAIChatRequestToGeminiKeepsTrailingFunctionCall(t *testing.T) {
	got, err := OpenAIChatRequestToGeminiGenerateContent(context.Background(), dto.GeneralOpenAIRequest{
		Model: "gemini-4-argon",
		Messages: []dto.Message{
			{Role: "user", Content: "Look this up."},
			{
				Role:      "assistant",
				Content:   "",
				ToolCalls: []byte(`[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]`),
			},
		},
	}, nil)
	require.NoError(t, err)
	require.Len(t, got.Contents, 2)
	assert.Equal(t, "model", got.Contents[1].Role)
	require.NotNil(t, got.Contents[1].Parts[0].FunctionCall)
	assert.Equal(t, "lookup", got.Contents[1].Parts[0].FunctionCall.FunctionName)
}
