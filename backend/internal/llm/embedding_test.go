package llm

import (
	"context"
	"encoding/json"
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

func TestOpenAIEmbeddingUsesBatchAndResponseIndexes(t *testing.T) {
	var request struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	httpClient := &http.Client{Transport: embeddingRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://provider.example/v1/embeddings" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request = %s, authorization = %q", r.URL.String(), r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`)),
		}, nil
	})}

	client := NewOpenAIEmbedding("https://provider.example/v1", "secret", "embedding-demo")
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
