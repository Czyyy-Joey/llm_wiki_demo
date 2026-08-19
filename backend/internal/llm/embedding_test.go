package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type embeddingRoundTripFunc func(*http.Request) (*http.Response, error)

func (f embeddingRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestOllamaEmbeddingUsesBatchAndResponseOrder(t *testing.T) {
	var request struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	httpClient := &http.Client{Transport: embeddingRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "http://ollama.example/api/embed" || r.Header.Get("Authorization") != "" {
			t.Errorf("request = %s, authorization = %q", r.URL.String(), r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"embeddings":[[1,0],[0,1]]}`)),
		}, nil
	})}

	client := NewOllamaEmbedding("http://ollama.example", "embedding-demo")
	client.HTTP = httpClient
	vectors, err := client.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Model != "embedding-demo" || !reflect.DeepEqual(request.Input, []string{"first", "second"}) {
		t.Fatalf("request = %#v", request)
	}
	if !reflect.DeepEqual(vectors, [][]float32{{1, 0}, {0, 1}}) {
		t.Fatalf("vectors = %#v", vectors)
	}
}

func TestOllamaEmbeddingRejectsMalformedResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing vector", body: `{"embeddings":[[1,0]]}`},
		{name: "empty vector", body: `{"embeddings":[[1,0],[]]}`},
		{name: "invalid JSON", body: `{"embeddings":`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := NewOllamaEmbedding("http://ollama.example", "embedding-demo")
			client.HTTP = &http.Client{Transport: embeddingRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(test.body))}, nil
			})}
			if _, err := client.Embed(context.Background(), []string{"first", "second"}); err == nil {
				t.Fatal("Embed() error = nil")
			}
		})
	}
}

func TestOllamaEmbeddingReportsHTTPFailure(t *testing.T) {
	client := NewOllamaEmbedding("http://ollama.example", "embedding-demo")
	client.HTTP = &http.Client{Transport: embeddingRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{"error":"model not found"}`))}, nil
	})}
	if _, err := client.Embed(context.Background(), []string{"first"}); err == nil || !strings.Contains(fmt.Sprint(err), "model not found") {
		t.Fatalf("Embed() error = %v", err)
	}
}
