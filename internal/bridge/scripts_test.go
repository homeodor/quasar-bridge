package bridge

import (
	"context"
	"errors"
	"os"
	"testing"

	"quasarbridge/internal/llm"
)

type scriptHome struct {
	ids             []string
	findErr, runErr error
	finds, runs     int
}

func (h *scriptHome) Handle(context.Context, string) (string, error) {
	return "", errors.New("script sent to planner")
}
func (h *scriptHome) FindScripts(context.Context, string) ([]string, error) {
	h.finds++
	return h.ids, h.findErr
}
func (h *scriptHome) RunScript(_ context.Context, text, id string) (string, error) {
	h.runs++
	if text != "Хочу кашу" || id != "script.porridge" {
		return "", errors.New("wrong script selection")
	}
	return "Запускаю.", h.runErr
}
func TestScriptDecisionPrecedesLLMAndResponses(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("responses.json", []byte(`{"хочу кашу":"Текстовый ответ"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name            string
		ids             []string
		findErr, runErr error
		wantRuns        int
		wantErr         bool
	}{
		{name: "unique", ids: []string{"script.porridge"}, wantRuns: 1},
		{name: "ambiguous", ids: []string{"script.porridge", "script.other"}},
		{name: "lookup error", findErr: errors.New("inventory incomplete"), wantErr: true},
		{name: "execution error", ids: []string{"script.porridge"}, runErr: errors.New("readback failed"), wantRuns: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &scriptHome{ids: tc.ids, findErr: tc.findErr, runErr: tc.runErr}
			e := Engine{Home: h, LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
				t.Fatal("script reached LLM")
				return "", nil
			})}
			d := e.Decide(t.Context(), "Хочу кашу")
			if d.Route != "home" || h.runs != 0 {
				t.Fatal("decision actuated or chose cloud", d)
			}
			r, err := e.ResolveDecision(t.Context(), "Хочу кашу", d)
			if r.Route != "home" || (err != nil) != tc.wantErr || h.runs != tc.wantRuns || (!tc.wantErr && r.Answer == "") || r.Answer == "Текстовый ответ" {
				t.Fatal(r, err, h.runs)
			}
		})
	}
}
func TestScriptNonmatchKeepsExistingRouting(t *testing.T) {
	for _, tc := range []struct{ text, route string }{
		{"Хочу кашу", "yandex"}, {"Какая погода", "yandex"}, {"Напиши код", "llm"},
	} {
		h := &scriptHome{}
		calls := 0
		e := Engine{Home: h, LLM: completionFunc(func(_ context.Context, _ []llm.Message, structured bool) (string, error) {
			calls++
			if structured {
				return `{"route":"yandex"}`, nil
			}
			return "Ответ", nil
		})}
		r, err := e.Resolve(t.Context(), tc.text)
		if err != nil || r.Route != tc.route || h.finds != 1 || h.runs != 0 {
			t.Fatal(r, err, h)
		}
		if tc.text == "Хочу кашу" && calls != 1 {
			t.Fatal("classifier fallback missing")
		}
	}
}
func TestCancelledOrSupersededScriptDecisionNeverRuns(t *testing.T) {
	h := &scriptHome{ids: []string{"script.porridge"}}
	e := Engine{Home: h}
	ctx, cancel := context.WithCancel(t.Context())
	d := e.Decide(ctx, "Хочу кашу")
	cancel()
	if _, err := e.ResolveDecision(ctx, "Хочу кашу", d); !errors.Is(err, context.Canceled) || h.runs != 0 {
		t.Fatal(err, h.runs)
	}
}
