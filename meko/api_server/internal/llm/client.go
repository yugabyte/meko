package llm

import (
	"context"
	"fmt"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
)

type Client struct {
	model  string
	client openai.Client
}

func NewClient(apiKey, model string) (*Client, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("openai api key is required")
	}
	if model == "" {
		model = string(openai.ChatModelGPT4o)
	}
	return &Client{
		model:  model,
		client: openai.NewClient(option.WithAPIKey(apiKey)),
	}, nil
}

func (c *Client) Complete(ctx context.Context, prompt string) (string, error) {
	resp, err := c.client.Responses.New(ctx, responses.ResponseNewParams{
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String(prompt)},
		Model: openai.ChatModel(c.model),
	})
	if err != nil {
		return "", fmt.Errorf("llm complete: %w", err)
	}
	return resp.OutputText(), nil
}

func (c *Client) GenerateEmbedding(ctx context.Context, text, embeddingModel string) ([]float64, error) {
	if embeddingModel == "" {
		embeddingModel = string(openai.EmbeddingModelTextEmbedding3Small)
	}
	resp, err := c.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Input:          openai.EmbeddingNewParamsInputUnion{OfString: openai.String(text)},
		Model:          openai.EmbeddingModel(embeddingModel),
		EncodingFormat: openai.EmbeddingNewParamsEncodingFormatFloat,
	})
	if err != nil {
		return nil, fmt.Errorf("generate embedding: %w", err)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("generate embedding: empty response")
	}
	return resp.Data[0].Embedding, nil
}
