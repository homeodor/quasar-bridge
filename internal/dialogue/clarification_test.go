package dialogue

import (
	"context"
	"testing"
	"time"
)

func TestContinuationExpiresSeparatesSpeakersAndClears(t *testing.T) {
	store := New(DefaultConfig())
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	a := WithSpeaker(t.Context(), "a")
	b := WithSpeaker(t.Context(), "b")
	_, ticket := store.Begin(a)
	if !store.CommitWithContinuation(a, ticket, "Включи свет", "В какой комнате?", false, "home") {
		t.Fatal("commit failed")
	}
	if view := store.View(a); view.AwaitingRoute != "home" || len(view.Messages) != 2 {
		t.Fatal(view)
	}
	if view := store.View(b); view.AwaitingRoute != "" || len(view.Messages) != 0 {
		t.Fatal("speaker leak", view)
	}
	now = now.Add(59 * time.Second)
	if view := store.View(a); view.AwaitingRoute != "home" {
		t.Fatal("expired too early", view)
	}
	now = now.Add(time.Second)
	if view, _ := store.Begin(a); view.AwaitingRoute != "" || len(view.Messages) != 0 {
		t.Fatal("expired question survived new request", view)
	}
	_, ticket = store.Begin(a)
	store.CommitWithContinuation(a, ticket, "Включи свет", "Где?", false, "home")
	if !store.Clear(a, ticket) || store.View(a).AwaitingRoute != "" {
		t.Fatal("clear retained question")
	}
}
func TestStaleAndCancelledContinuationCannotRearm(t *testing.T) {
	store := New(DefaultConfig())
	ctx := WithSpeaker(t.Context(), "speaker")
	_, old := store.Begin(ctx)
	_, current := store.Begin(ctx)
	if store.CommitWithContinuation(ctx, old, "old", "Where?", false, "home") {
		t.Fatal("stale question rearmed")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if store.CommitWithContinuation(cancelled, current, "old", "Where?", false, "home") {
		t.Fatal("cancelled question rearmed")
	}
	if !store.Commit(ctx, current, "new", "Done", false) || store.View(ctx).AwaitingRoute != "" {
		t.Fatal("completed answer kept question")
	}
}
func TestContinuationRetainsOriginalAcrossSeveralClarifications(t *testing.T) {
	store := New(DefaultConfig())
	ctx := t.Context()
	for _, turn := range [][2]string{{"Включи свет", "В какой комнате?"}, {"В спальне", "Какой светильник?"}} {
		_, ticket := store.Begin(ctx)
		if !store.CommitWithContinuation(ctx, ticket, turn[0], turn[1], false, "home") {
			t.Fatal("commit failed")
		}
	}
	v := store.View(ctx)
	if v.AwaitingRoute != "home" || len(v.Messages) != 4 || v.Messages[0].Content != "Включи свет" {
		t.Fatal(v)
	}
}
func TestDisabledDialogueDoesNotRetainContinuation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = false
	s := New(cfg)
	_, ticket := s.Begin(t.Context())
	s.CommitWithContinuation(t.Context(), ticket, "user", "question?", false, "home")
	if v := s.View(t.Context()); v.AwaitingRoute != "" || len(v.Messages) != 0 {
		t.Fatal(v)
	}
}
func TestWantsReplyUsesFinalQuestion(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{{"В какой комнате?", true}, {"Какой светильник?  ", true}, {"Готово.", false}, {"Запускаю.", false}, {"Вы спрашивали почему? Сейчас объясню.", false}} {
		if got := WantsReply(tc.text); got != tc.want {
			t.Fatalf("%q: %v", tc.text, got)
		}
	}
}

func TestExpiredLLMQuestionKeepsNormalHistoryTTL(t *testing.T) {
	s := New(DefaultConfig())
	now := time.Now()
	s.now = func() time.Time { return now }
	_, ticket := s.Begin(t.Context())
	s.CommitWithContinuation(t.Context(), ticket, "вопрос", "Уточните?", false, "llm")
	now = now.Add(time.Minute)
	if v := s.View(t.Context()); v.AwaitingRoute != "" || len(v.Messages) != 2 {
		t.Fatal("normal LLM history lost with waiting marker", v)
	}
	now = now.Add(4 * time.Minute)
	if v := s.View(t.Context()); len(v.Messages) != 0 {
		t.Fatal("history outlived configured TTL", v)
	}
}
