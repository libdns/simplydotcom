package simplydotcom

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/libdns/libdns"
)

func TestClient_ResolvesProductObject(t *testing.T) {
	var requests []string
	productLists := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected Authorization header %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/my/products/" {
			productLists++
			json.NewEncoder(w).Encode(map[string]any{"products": []map[string]any{
				{"object": "kvolbæk.dk", "domain": map[string]any{"name": "kvolbæk.dk", "name_idn": "xn--kvolbk-tua.dk"}},
				{"object": "example.com", "domain": map[string]any{"name": "example.com", "name_idn": "example.com"}},
				{"object": "www.example.com", "domain": map[string]any{"name": "www.example.com", "name_idn": "www.example.com"}},
			}})
			return
		}
		requests = append(requests, r.Method+" "+r.RequestURI)
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"records": []map[string]any{
				{"record_id": 7, "name": "_acme-challenge", "type": "TXT", "data": "token", "ttl": 300},
			}})
		case http.MethodPost:
			json.NewEncoder(w).Encode(map[string]any{"record": map[string]any{"id": 7}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"status": 200})
		}
	}))
	defer server.Close()

	provider := &Provider{APIKey: "secret", BaseURL: server.URL + "/"}
	ctx := context.Background()
	record := libdns.TXT{Name: "_acme-challenge", Text: "token", TTL: 300 * time.Second}

	if _, err := provider.AppendRecords(ctx, "xn--kvolbk-tua.dk.", []libdns.Record{record}); err != nil {
		t.Fatalf("AppendRecords: %v", err)
	}
	if _, err := provider.SetRecords(ctx, "KVOLBÆK.DK.", []libdns.Record{record}); err != nil {
		t.Fatalf("SetRecords: %v", err)
	}
	if _, err := provider.DeleteRecords(ctx, "www.example.com.", []libdns.Record{record}); err != nil {
		t.Fatalf("DeleteRecords: %v", err)
	}

	expected := []string{
		"POST /my/products/kvolb%C3%A6k.dk/dns/records/",
		"GET /my/products/kvolb%C3%A6k.dk/dns/records/",
		"GET /my/products/kvolb%C3%A6k.dk/dns/records/",
		"PUT /my/products/kvolb%C3%A6k.dk/dns/records/7/",
		"GET /my/products/kvolb%C3%A6k.dk/dns/records/",
		"GET /my/products/www.example.com/dns/records/",
		"DELETE /my/products/www.example.com/dns/records/7/",
	}
	if strings.Join(requests, "\n") != strings.Join(expected, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(requests, "\n"), strings.Join(expected, "\n"))
	}
	if productLists != 1 {
		t.Errorf("expected products to be listed once, got %d", productLists)
	}

	if _, err := provider.GetRecords(ctx, "unknown.dk."); err == nil {
		t.Error("expected error for a zone without a product")
	}
}
