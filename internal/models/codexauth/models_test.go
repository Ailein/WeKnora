package codexauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const sampleModelsJSON = `{"models":[
  {"slug":"gpt-5.6-sol","display_name":"GPT-5.6-Sol","description":"frontier","visibility":"list","supported_in_api":true,"input_modalities":["text","image"]},
  {"slug":"gpt-reserve","display_name":"GPT-Reserve","visibility":"hide","supported_in_api":true,"input_modalities":["text","image"]},
  {"slug":"gpt-text-only","display_name":"Text Only","visibility":"list","supported_in_api":true,"input_modalities":["text"]},
  {"slug":"gpt-no-api","display_name":"No API","visibility":"list","supported_in_api":false,"input_modalities":["text"]}
]}`

func TestListModelsFiltersAndParses(t *testing.T) {
	var gotVersion, gotAuth, gotAccount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		gotVersion = r.URL.Query().Get("client_version")
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("chatgpt-account-id")
		fmt.Fprint(w, sampleModelsJSON)
	}))
	defer srv.Close()

	models, err := ListModels(context.Background(), srv.URL, "tok-1", "acc-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotVersion != modelsClientVersion {
		t.Fatalf("client_version = %q, want %q", gotVersion, modelsClientVersion)
	}
	if gotAuth != "Bearer tok-1" || gotAccount != "acc-1" {
		t.Fatalf("auth headers = %q / %q", gotAuth, gotAccount)
	}
	// hide、supported_in_api=false 都要滤掉
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(models), models)
	}
	if models[0].Slug != "gpt-5.6-sol" || !models[0].Vision || models[0].DisplayName != "GPT-5.6-Sol" {
		t.Fatalf("first model wrong: %+v", models[0])
	}
	if models[1].Slug != "gpt-text-only" || models[1].Vision {
		t.Fatalf("second model wrong: %+v", models[1])
	}
}

func TestListModelsRetriesTransportErrors(t *testing.T) {
	withFastRetries(t)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			hijackClose(t, w)
			return
		}
		fmt.Fprint(w, sampleModelsJSON)
	}))
	defer srv.Close()

	models, err := ListModels(context.Background(), srv.URL, "tok", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || len(models) == 0 {
		t.Fatalf("attempts=%d models=%d", attempts.Load(), len(models))
	}
}

func TestListModelsHTTPErrorNoRetry(t *testing.T) {
	withFastRetries(t)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"detail":"bad token"}`)
	}))
	defer srv.Close()

	_, err := ListModels(context.Background(), srv.URL, "tok", "acc")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("want HTTP 401 error, got %v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("HTTP errors must not retry, attempts=%d", attempts.Load())
	}
}

func TestListModelsEmptyCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[{"slug":"internal","visibility":"hide"}]}`)
	}))
	defer srv.Close()

	if _, err := ListModels(context.Background(), srv.URL, "tok", "acc"); err == nil {
		t.Fatal("want error for catalog with no listable models")
	}
}
