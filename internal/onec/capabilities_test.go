package onec

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// capsServer поднимает заглушку 1С, отвечающую заданным телом на /mcp/health,
// и считает обращения — по счётчику видно, работает ли кэш.
func capsServer(t *testing.T, body string) (*Client, *atomic.Int32) {
	t.Helper()

	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/health" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	client := NewClient(Settings{
		BaseURL:       srv.URL,
		Timeout:       2 * time.Second,
		ReportTimeout: 2 * time.Second,
	}, slog.New(slog.DiscardHandler))

	return client, &hits
}

const healthWithProfile = `{
	"status": "ok",
	"time": "2026-09-03T10:00:00",
	"capabilities": {
		"profile": "upp-1.3",
		"version": 2,
		"unsupported": {"cash_flow": {"filters": ["cost_article_ids"]}},
		"extra": {"production_consumption": {"group_by": ["cost_article"]}},
		"tools": {"available": ["cash_flow", "production_consumption"]},
		"resolvers": {"always_empty": ["material"]}
	}
}`

func TestCapabilitiesParsesProfile(t *testing.T) {
	client, _ := capsServer(t, healthWithProfile)

	caps := client.Capabilities(context.Background())
	if caps == nil {
		t.Fatal("capabilities are nil for a health response that carries a profile")
	}

	if caps.Profile != "upp-1.3" {
		t.Errorf("profile = %q, want upp-1.3", caps.Profile)
	}

	facets, ok := caps.Unsupported["cash_flow"]
	if !ok || len(facets.Filters) != 1 || facets.Filters[0] != "cost_article_ids" {
		t.Errorf("unsupported.cash_flow parsed as %+v", facets)
	}

	if got := caps.Extra["production_consumption"].GroupBy; len(got) != 1 || got[0] != "cost_article" {
		t.Errorf("extra.production_consumption.group_by = %v", got)
	}

	if !caps.ToolAvailable("cash_flow") || caps.ToolAvailable("goods_in_transit") {
		t.Errorf("tools.available parsed as %v", caps.Tools.Available)
	}

	if len(caps.Resolvers.AlwaysEmpty) != 1 || caps.Resolvers.AlwaysEmpty[0] != "material" {
		t.Errorf("resolvers.always_empty = %v", caps.Resolvers.AlwaysEmpty)
	}
}

// tools/list зовётся на каждую сессию модели, а профиль меняется вместе с кодом 1С.
// Без кэша каждый список инструментов тащил бы за собой поход в 1С.
func TestCapabilitiesAreCached(t *testing.T) {
	client, hits := capsServer(t, healthWithProfile)

	for range 5 {
		if client.Capabilities(context.Background()) == nil {
			t.Fatal("capabilities went nil mid-run")
		}
	}

	if got := hits.Load(); got != 1 {
		t.Errorf("1C hit %d times, want 1 — the profile is not cached", got)
	}
}

// 1С ответила, но профиля не отдала: база ничего не подтвердила. Раньше это был fail-open,
// и так до модели доезжали инструменты, которых в базе нет.
func TestCapabilitiesAbsentProfileConfirmsNothing(t *testing.T) {
	client, _ := capsServer(t, `{"status":"ok","time":"2026-09-03T10:00:00"}`)

	caps := client.Capabilities(context.Background())
	if caps == nil {
		t.Fatal("expected an empty profile for a health without capabilities, got nil (unknown)")
	}

	if caps.ToolAvailable("stock_balance") {
		t.Error("a database that published no profile has a confirmed tool")
	}
}

// Незнакомую версию не применяем наполовину — и не показываем по ней всё подряд.
func TestCapabilitiesVersionMismatch(t *testing.T) {
	client, _ := capsServer(t,
		`{"status":"ok","capabilities":{"profile":"upp-1.3","version":99,"tools":{"available":["stock_balance"]}}}`)

	caps := client.Capabilities(context.Background())
	if caps == nil {
		t.Fatal("expected an empty profile for version 99, got nil")
	}

	if caps.ToolAvailable("stock_balance") {
		t.Error("a profile of an unknown version was partially applied")
	}
}

// Переходный период: профиль версии 1 ещё читается.
func TestCapabilitiesLegacyVersion1(t *testing.T) {
	client, _ := capsServer(t,
		`{"status":"ok","capabilities":{"version":1,"tools":{"unavailable":["goods_in_transit"]}}}`)

	caps := client.Capabilities(context.Background())
	if caps == nil || caps.Version != 1 {
		t.Fatalf("version 1 profile not accepted: %+v", caps)
	}

	if !caps.ToolAvailable("stock_balance") || caps.ToolAvailable("goods_in_transit") {
		t.Error("version 1 profile applied incorrectly")
	}
}

// Недоступная 1С без ранее полученного профиля: профиль неизвестен (nil), выдача не
// сужается. И повторные вызовы не ждут сетевого таймаута.
func TestCapabilitiesUnknownWhenUnreachable(t *testing.T) {
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	client := NewClient(Settings{
		BaseURL:       srv.URL,
		Timeout:       2 * time.Second,
		ReportTimeout: 2 * time.Second,
	}, slog.New(slog.DiscardHandler))

	if caps := client.Capabilities(context.Background()); caps != nil {
		t.Errorf("expected nil capabilities when 1C is down, got %+v", caps)
	}

	client.Capabilities(context.Background())

	if got := hits.Load(); got != 1 {
		t.Errorf("1C hit %d times after a failure, want 1 — the failure is not cached", got)
	}
}

// Сбой связи ничего не говорит о возможностях базы: последний полученный профиль остаётся
// в силе, иначе на время сбоя к модели вернулись бы все неподтверждённые инструменты.
func TestCapabilitiesKeepLastProfileOnFailure(t *testing.T) {
	var down atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(healthWithProfile))
	}))
	t.Cleanup(srv.Close)

	client := NewClient(Settings{
		BaseURL:       srv.URL,
		Timeout:       2 * time.Second,
		ReportTimeout: 2 * time.Second,
	}, slog.New(slog.DiscardHandler))

	if client.Capabilities(context.Background()) == nil {
		t.Fatal("initial profile not received")
	}

	down.Store(true)
	client.capsMu.Lock()
	client.capsExpire = time.Time{}
	client.capsMu.Unlock()

	caps := client.Capabilities(context.Background())
	if caps == nil || caps.Profile != "upp-1.3" {
		t.Fatalf("last known profile lost on a 1C failure: %+v", caps)
	}

	if caps.ToolAvailable("goods_in_transit") {
		t.Error("an unconfirmed tool became available during a 1C outage")
	}
}
