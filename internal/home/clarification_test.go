package home

import (
	"context"
	"errors"
	"quasarbridge/internal/dialogue"
	"testing"

	"quasarbridge/internal/llm"
)

func TestHomeDialogueUsesHistoryAndFreshValidatedTargets(t *testing.T) {
	f := &homeFixture{state: "off"}
	calls := 0
	c := Controller{Tools: f, Planner: plannerFunc(func(_ context.Context, m []llm.Message, structured bool) (string, error) {
		if !structured {
			t.Fatal("action request used readonly planner")
		}
		calls++
		if calls == 1 {
			return `{"answer":"Уточните комнату.","needs_clarification":true,"actions":[]}`, nil
		}
		if len(m) != 4 || m[1].Content != "Включи свет" || m[2].Content != "Уточните комнату." {
			t.Fatal("original request lost", m)
		}
		return `{"answer":"Готово","needs_clarification":false,"actions":[{"domain":"light","service":"turn_on","entity_id":"light.office","data":{}}]}`, nil
	})}
	first, err := c.HandleDialogue(t.Context(), "Включи свет", nil)
	if err != nil || !first.ShouldListen || f.actions != 0 {
		t.Fatal(first, err, f.actions)
	}
	second, err := c.HandleDialogue(t.Context(), "в кабинете", []llm.Message{{Role: "user", Content: "Включи свет"}, {Role: "assistant", Content: first.Answer}})
	if err != nil || second.ShouldListen || f.actions != 1 {
		t.Fatal(second, err, f.actions)
	}
}
func TestHomeClarificationCannotIncludeActions(t *testing.T) {
	f := &homeFixture{state: "off"}
	c := Controller{Tools: f, Domains: []string{"light"}, Planner: plannerFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return `{"answer":"Включить?","needs_clarification":true,"actions":[{"domain":"light","service":"turn_on","entity_id":"light.office","data":{}}]}`, nil
	})}
	if _, err := c.HandleDialogue(t.Context(), "Включи свет", nil); err == nil || f.actions != 0 {
		t.Fatal("question actuated", err, f.actions)
	}
}
func TestHomeClarificationDoesNotBypassExposure(t *testing.T) {
	f := &homeFixture{state: "off", notExposed: true}
	c := Controller{Tools: f, Domains: []string{"light"}, Planner: plannerFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Fatal("hidden entity reached planner")
		return "", nil
	})}
	if _, err := c.HandleDialogue(t.Context(), "в кабинете", []llm.Message{{Role: "user", Content: "Включи свет"}, {Role: "assistant", Content: "Где?"}}); err != nil || f.actions != 0 {
		t.Fatal(err, f.actions)
	}
}

func TestReadOnlyHomeQuestionCanRequestClarificationWithoutActuating(t *testing.T) {
	f := &homeFixture{state: "off"}
	c := Controller{Tools: f, Domains: []string{"light"}, Planner: plannerFunc(func(_ context.Context, m []llm.Message, structured bool) (string, error) {
		if structured || len(m) != 4 || m[1].Content != "Горит ли свет?" {
			t.Fatal("readonly context lost", structured, m)
		}
		return "Какой светильник?", nil
	})}
	r, err := c.HandleDialogue(t.Context(), "в кабинете", []llm.Message{{Role: "user", Content: "Горит ли свет?"}, {Role: "assistant", Content: "В какой комнате?"}})
	if err != nil || !r.ShouldListen || f.actions != 0 {
		t.Fatal(r, err, f.actions)
	}
}

func TestSupersededHomeClarificationNeverExecutesLatePlan(t *testing.T) {
	f := &homeFixture{state: "off"}
	store := dialogue.New(dialogue.DefaultConfig())
	ctx, _, _ := store.Start(t.Context())
	c := Controller{Tools: f, Domains: []string{"light"}, Planner: plannerFunc(func(context.Context, []llm.Message, bool) (string, error) {
		store.Begin(t.Context()) // Another utterance superseded this planner call.
		return `{"answer":"Готово","actions":[{"domain":"light","service":"turn_on","entity_id":"light.office","data":{}}]}`, nil
	})}
	if _, err := c.HandleDialogue(ctx, "в кабинете", []llm.Message{{Role: "user", Content: "Включи свет"}, {Role: "assistant", Content: "Где?"}}); !errors.Is(err, context.Canceled) || f.actions != 0 {
		t.Fatal("late plan actuated", err, f.actions)
	}
}
