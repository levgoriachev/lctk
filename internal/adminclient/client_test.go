package adminclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lev-goryachev/lctk/internal/adminsession"
)

func TestNativeSessionCarriesProcessMemoryHeadersWithoutABrowser(t *testing.T) {
	store, err := adminsession.New(adminsession.Options{Path: t.TempDir() + "/admin.json"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/session", func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		token, csrf, err := store.Exchange(body["code"])
		if err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(writer).Encode(map[string]string{"session": token, "csrf": csrf})
	})
	mux.HandleFunc("GET /admin/api/overview", func(writer http.ResponseWriter, request *http.Request) {
		if _, err := store.Authenticate(request); err != nil {
			http.Error(writer, err.Error(), http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(writer).Encode(Overview{Version: "test"})
	})
	mux.HandleFunc("GET /admin/api/projects", func(writer http.ResponseWriter, request *http.Request) {
		if _, err := store.Authenticate(request); err != nil {
			http.Error(writer, err.Error(), http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"projects": []Project{{
			ID: "alpha-aaaaaaaa", Index: &Index{Semantic: &SemanticIndex{
				Indexing: true, ChunksEmbedded: 96, ChunksReused: 4, ChunksTotal: 1161,
			}},
		}}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := Connect(t.Context(), strings.TrimPrefix(server.URL, "http://"), store.Code())
	if err != nil {
		t.Fatal(err)
	}
	var overview Overview
	if err := client.do(t.Context(), http.MethodGet, "/admin/api/overview", nil, &overview, true); err != nil {
		t.Fatal(err)
	}
	if overview.Version != "test" {
		t.Fatalf("version=%q", overview.Version)
	}
	projects, err := client.LoadProjects(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Index == nil || projects[0].Index.Semantic == nil {
		t.Fatalf("projects = %+v, want one semantic project", projects)
	}
	semantic := projects[0].Index.Semantic
	if semantic.ChunksEmbedded+semantic.ChunksReused != 100 || semantic.ChunksTotal != 1161 {
		t.Fatalf("semantic = %+v, want live progress fields", semantic)
	}
}

func TestNativeSessionRefusesANonLoopbackAddressBeforeSendingTheCode(t *testing.T) {
	if _, err := Connect(t.Context(), "example.com:4444", "secret"); err == nil {
		t.Fatal("a native administrator credential could be sent outside loopback")
	}
}
