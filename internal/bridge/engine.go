package bridge

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"quasarbridge/internal/dialogue"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quasarbridge/internal/llm"
)

type Completer interface {
	Complete(context.Context, []llm.Message, bool) (string, error)
}
type HomeHandler interface {
	Handle(context.Context, string) (string, error)
}

// ScriptHandler discovers only Assist-exposed scripts and runs a selected ID.
// Discovery never actuates; RunScript must revalidate the selection.
type ScriptHandler interface {
	FindScripts(context.Context, string) ([]string, error)
	RunScript(context.Context, string, string) (string, error)
}
type Engine struct {
	LLM          Completer
	Assistant    Completer
	Home         HomeHandler
	Dialogue     *dialogue.Store
	dialogueOnce sync.Once
	routing      atomic.Pointer[RoutingPolicy]
}

func (e *Engine) dialogue() *dialogue.Store {
	e.dialogueOnce.Do(func() {
		if e.Dialogue == nil {
			e.Dialogue = dialogue.New(dialogue.DefaultConfig())
		}
	})
	return e.Dialogue
}

// Prepare reserves this speaker's request before ASR/classification can finish late.
func (e *Engine) Prepare(ctx context.Context) context.Context {
	if ctx.Err() != nil {
		return ctx
	}
	ctx, _, _ = e.dialogue().Start(ctx)
	return ctx
}

func (e *Engine) Resolve(ctx context.Context, text string) (Resolution, error) {
	return e.ResolveDecision(ctx, text, e.Decide(ctx, text))
}
func exactResponse(text string) (string, bool) {
	data, err := os.ReadFile("responses.json")
	if err != nil {
		return "", false
	}

	var responses map[string]string
	if json.Unmarshal(data, &responses) != nil {
		return "", false
	}

	query := strings.ToLower(strings.TrimSpace(text))
	query = strings.TrimRight(query, "?!.")

	for input, output := range responses {
		key := strings.ToLower(strings.TrimSpace(input))
		key = strings.TrimRight(key, "?!.")
		if query == key {
			return output, true
		}
	}

	return "", false
}

func (e *Engine) ResolveDecision(ctx context.Context, text string, decision RouteDecision) (Resolution, error) {
	route := decision.Route
	if decision.prepared {
		ctx = e.dialogue().Use(ctx, decision.ticket)
	} else {
		ctx = e.Prepare(ctx)
	}
	_, _, ticket := e.dialogue().Start(ctx)
	if ctx.Err() != nil {
		return Resolution{Route: route}, ctx.Err()
	}

	if !e.dialogue().Current(ctx, ticket) {
		return Resolution{Route: route}, context.Canceled
	}
	if decision.Source == "script_exact" || decision.Source == "script_lookup_error" {
		result := Resolution{Route: "home"}
		if !e.dialogue().Clear(ctx, ticket) {
			return result, context.Canceled
		}
		if decision.scriptErr != nil {
			return result, decision.scriptErr
		}
		if len(decision.scriptIDs) != 1 {
			result.Answer = "Нашлось несколько скриптов с таким названием. Задайте им разные названия в Home Assistant."
			return result, nil
		}
		handler, ok := e.Home.(ScriptHandler)
		if !ok {
			return result, errors.New("script handler is not configured")
		}
		answer, err := handler.RunScript(ctx, text, decision.scriptIDs[0])
		result.Answer = answer
		return result, err
	}
	if answer, ok := exactResponse(text); ok {
		return Resolution{Route: "llm", Answer: answer}, nil
	}
	if decision.Source == "dialogue_reset" {
		if !e.dialogue().Clear(ctx, ticket) {
			return Resolution{Route: route}, context.Canceled
		}
		return Resolution{Route: "llm", Answer: "Начинаем новый разговор."}, nil
	}
	if route != "llm" && !e.dialogue().Clear(ctx, ticket) {
		return Resolution{Route: route}, context.Canceled
	}
	result := Resolution{Route: route}
	switch route {
	case "home":
		if e.Home == nil {
			return Resolution{Route: "yandex"}, nil
		}
		answer, err := e.Home.Handle(ctx, text)
		result.Answer = answer
		return result, err
	case "llm":
		return e.Fallback(ctx, text)
	default:
		return Resolution{Route: "yandex"}, nil
	}
}
func (e *Engine) Fallback(ctx context.Context, text string) (Resolution, error) {
	r := Resolution{Route: "llm"}
	if e.LLM == nil && e.Assistant == nil {
		return r, errors.New("LLM is not configured")
	}
	answerer := e.LLM
	if e.Assistant != nil {
		answerer = e.Assistant
	}
	ctx, snapshot, ticket := e.dialogue().Start(ctx)
	if !e.dialogue().Current(ctx, ticket) {
		return r, context.Canceled
	}
	messages := []llm.Message{{Role: "system", Content: "Ты голосовой помощник пользователя. Отвечай по-русски, кратко и по существу, обычно до трёх предложений. Ответ будет прочитан вслух: без Markdown и служебных рассуждений. Выполняй внешние действия только доступными инструментами по явному запросу пользователя. Не заявляй о выполнении действий без успешного результата инструмента; если инструмента нет, прямо скажи об этом. Для Home Assistant есть отдельный обработчик умного дома."}}
	messages = append(messages, snapshot.Messages...)
	messages = append(messages, llm.Message{Role: "user", Content: text})
	private := snapshot.PrivateHistory
	var answer string
	var err error
	if contextual, ok := answerer.(interface {
		CompleteDialogue(context.Context, []llm.Message, bool) (string, bool, error)
	}); ok {
		answer, private, err = contextual.CompleteDialogue(ctx, messages, private)
	} else {
		answer, err = answerer.Complete(ctx, messages, false)
	}
	if !e.dialogue().Current(ctx, ticket) {
		return r, context.Canceled
	}
	if err == nil {
		if !e.dialogue().Commit(ctx, ticket, text, answer, private) {
			return r, context.Canceled
		}
	}
	r.Answer = answer
	return r, err
}
func containsOne(text string, needles []string) bool {
	for _, s := range needles {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}
func (e *Engine) SetRouting(cfg RoutingConfig) error {
	p, err := CompileRouting(cfg)
	if err != nil {
		return err
	}
	e.routing.Store(p)
	return nil
}
func (e *Engine) policy() *RoutingPolicy {
	if p := e.routing.Load(); p != nil {
		return p
	}
	return builtInRouting
}

type RoutingStatus struct {
	Revision    string
	ActiveRules int
}

func (e *Engine) RoutingStatus() RoutingStatus {
	p := e.policy()
	return RoutingStatus{Revision: p.revision, ActiveRules: len(p.rules)}
}
func (e *Engine) IsRefusal(text string) bool { return e.policy().isRefusal(text) }
func IsRefusal(text string) bool             { return builtInRouting.isRefusal(text) }
func (e *Engine) Decide(ctx context.Context, text string) (decision RouteDecision) {
	ctx, snapshot, ticket := e.dialogue().Start(ctx)
	defer func() {
		// Choose the cloud path before Native can start local progress/TTS.
		if decision.Route == "home" && e.Home == nil {
			decision.Route = "yandex"
			decision.Source = "home_disabled"
		}
		decision.ticket = ticket
		decision.prepared = true
	}()
	if ctx.Err() != nil || !e.dialogue().Current(ctx, ticket) {
		return RouteDecision{Route: "home", Source: "script_lookup_error", scriptErr: context.Canceled}
	}
	if scripts, ok := e.Home.(ScriptHandler); ok {
		ids, err := scripts.FindScripts(ctx, text)
		if err != nil {
			// An incomplete inventory cannot establish a safe nonmatch.
			return RouteDecision{Route: "home", Source: "script_lookup_error", scriptErr: err}
		}
		if len(ids) > 0 {
			return RouteDecision{Route: "home", Source: "script_exact", scriptIDs: ids}
		}
	}
	p := e.policy()
	if dialogue.IsReset(text) {
		return RouteDecision{Route: "llm", Source: "dialogue_reset"}
	}
	if d, ok := p.match(text); ok {
		return d
	}
	if len(snapshot.Messages) > 0 && dialogue.IsFollowup(text) {
		return RouteDecision{Route: "llm", Source: "dialogue"}
	}
	if p.defaultRoute != "classify" {
		return RouteDecision{Route: p.defaultRoute, Source: "default"}
	}
	failed := RouteDecision{Route: p.classifier.OnError, Source: "classifier_error"}
	if e.LLM == nil {
		return failed
	}
	classCtx, cancel := context.WithTimeout(ctx, time.Duration(p.classifier.TimeoutMS)*time.Millisecond)
	defer cancel()
	messages := []llm.Message{{Role: "system", Content: p.classifier.Prompt}}
	messages = append(messages, snapshot.Messages...)
	messages = append(messages, llm.Message{Role: "user", Content: text})
	answer, err := e.LLM.Complete(classCtx, messages, true)
	if err != nil {
		return failed
	}
	var choice struct {
		Route string `json:"route"`
	}
	if json.Unmarshal([]byte(answer), &choice) != nil || !directRoute(choice.Route) {
		return failed
	}
	return RouteDecision{Route: choice.Route, Source: "classifier"}
}
