package bridge

import (
	"context"
	"errors"
	"testing"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
)

type dialogueHomeFunc func(context.Context, string, []llm.Message) (dialogue.Reply, error)

func (f dialogueHomeFunc) Handle(context.Context, string) (string, error) {
	return "", errors.New("legacy home path used")
}
func (f dialogueHomeFunc) HandleDialogue(ctx context.Context, text string, m []llm.Message) (dialogue.Reply, error) {
	return f(ctx, text, m)
}

func TestHomeClarificationKeepsOriginalAndStopsAfterAction(t *testing.T) {
	calls := 0
	e := Engine{Home: dialogueHomeFunc(func(_ context.Context, text string, history []llm.Message) (dialogue.Reply, error) {
		calls++
		switch calls {
		case 1:
			if text != "Включи свет" || len(history) != 0 {
				t.Fatal(text, history)
			}
			return dialogue.Reply{Answer: "В какой комнате?", ShouldListen: true}, nil
		case 2:
			if text != "в спальне" || len(history) != 2 || history[0].Content != "Включи свет" {
				t.Fatal(text, history)
			}
			return dialogue.Reply{Answer: "Какой светильник?", ShouldListen: true}, nil
		default:
			if text != "потолочный" || len(history) != 4 || history[0].Content != "Включи свет" {
				t.Fatal(text, history)
			}
			return dialogue.Reply{Answer: "Готово."}, nil
		}
	}), LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Fatal("clarification reached LLM classifier")
		return "", nil
	})}
	for i, text := range []string{"Включи свет", "в спальне", "потолочный"} {
		r, err := e.Resolve(t.Context(), text)
		if err != nil || r.Route != "home" || r.ShouldListen != (i < 2) {
			t.Fatal(r, err)
		}
	}
	if v := e.dialogue().View(t.Context()); len(v.Messages) != 0 || v.AwaitingRoute != "" {
		t.Fatal("completed action retained", v)
	}
}
func TestHomeClarificationIsScopedAndNewCommandsCancelIt(t *testing.T) {
	for _, text := range []string{"отмена", "не надо", "начни новый разговор", "Какая погода", "Включи телевизор", "Напиши код"} {
		t.Run(text, func(t *testing.T) {
			calls := 0
			e := Engine{Home: dialogueHomeFunc(func(_ context.Context, _ string, history []llm.Message) (dialogue.Reply, error) {
				calls++
				if len(history) != 0 {
					t.Fatal("new command reused unfinished action", history)
				}
				if calls == 1 {
					return dialogue.Reply{Answer: "В какой комнате?", ShouldListen: true}, nil
				}
				return dialogue.Reply{Answer: "Готово."}, nil
			}), LLM: completionFunc(func(_ context.Context, m []llm.Message, _ bool) (string, error) {
				for _, msg := range m {
					if msg.Content == "Включи свет" {
						t.Fatal("abandoned HA request leaked into LLM")
					}
				}
				return "Ответ", nil
			})}
			if r, err := e.Resolve(t.Context(), "Включи свет"); err != nil || !r.ShouldListen {
				t.Fatal(r, err)
			}
			r, err := e.Resolve(t.Context(), text)
			if err != nil || r.ShouldListen {
				t.Fatal(r, err)
			}
			want := 1
			if text == "Включи телевизор" {
				want = 2
			}
			if calls != want || e.dialogue().View(t.Context()).AwaitingRoute != "" {
				t.Fatal("pending action survived", calls)
			}
		})
	}
	e := Engine{Home: dialogueHomeFunc(func(context.Context, string, []llm.Message) (dialogue.Reply, error) {
		return dialogue.Reply{Answer: "Где?", ShouldListen: true}, nil
	})}
	a := dialogue.WithSpeaker(t.Context(), "a")
	b := dialogue.WithSpeaker(t.Context(), "b")
	e.Resolve(a, "Включи свет")
	if d := e.Decide(b, "в спальне"); d.Source == "home_clarification" {
		t.Fatal("speaker inherited pending action", d)
	}
}
func TestLLMQuestionAcceptsArbitraryShortAnswer(t *testing.T) {
	calls := 0
	e := Engine{LLM: completionFunc(func(_ context.Context, m []llm.Message, structured bool) (string, error) {
		if structured {
			t.Fatal("pending answer reached classifier")
		}
		calls++
		if calls == 1 {
			return "В какой комнате?", nil
		}
		if len(m) != 4 || m[1].Content != "спроси нейросеть: помоги выбрать освещение" || m[3].Content != "в спальне" {
			t.Fatal(m)
		}
		return "Подойдёт тёплый свет.", nil
	})}
	if r, err := e.Resolve(t.Context(), "спроси нейросеть: помоги выбрать освещение"); err != nil || !r.ShouldListen {
		t.Fatal(r, err)
	}
	if r, err := e.Resolve(t.Context(), "в спальне"); err != nil || r.Route != "llm" || r.ShouldListen {
		t.Fatal(r, err)
	}
}
func TestFailedOrStaleHomeQuestionDoesNotRearm(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		e := Engine{Home: dialogueHomeFunc(func(context.Context, string, []llm.Message) (dialogue.Reply, error) {
			return dialogue.Reply{Answer: "Где?", ShouldListen: true}, errors.New("failure")
		})}
		if r, err := e.Resolve(t.Context(), "Включи свет"); err == nil || r.ShouldListen || e.dialogue().View(t.Context()).AwaitingRoute != "" {
			t.Fatal(r, err)
		}
	})
	t.Run("stale", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		e := Engine{Home: dialogueHomeFunc(func(context.Context, string, []llm.Message) (dialogue.Reply, error) {
			close(entered)
			<-release
			return dialogue.Reply{Answer: "Где?", ShouldListen: true}, nil
		})}
		done := make(chan error, 1)
		go func() { _, err := e.Resolve(t.Context(), "Включи свет"); done <- err }()
		<-entered
		if _, err := e.Resolve(t.Context(), "Какая погода"); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err := <-done; !errors.Is(err, context.Canceled) || e.dialogue().View(t.Context()).AwaitingRoute != "" {
			t.Fatal(err)
		}
	})
}
