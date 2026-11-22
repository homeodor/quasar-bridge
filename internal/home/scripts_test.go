package home

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"testing"
	"time"
)

type scriptFixture struct {
	entities  []Entity
	hidden    map[string]bool
	exposures int
	revoke    bool
	actions   int
	reads     int
	fail      string
	before    string
	after     string
	state     string
	rename    bool
}

func (f *scriptFixture) Call(ctx context.Context, tool string, args map[string]any) (jsontext.Value, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if f.fail == tool {
		return nil, errors.New("tool failure")
	}
	var v any
	switch tool {
	case "ha_search":
		if args["domain_filter"] != "script" || args["include_hidden"] != false {
			return nil, errors.New("unsafe discovery")
		}
		v = map[string]any{"entities": f.entities}
	case "ha_get_entity_exposure":
		f.exposures++
		visible := !f.hidden[args["entity_id"].(string)] && !(f.revoke && f.reads > 0)
		v = map[string]any{"exposed_to": map[string]bool{"conversation": visible}}
	case "ha_call_service":
		f.actions++
		if args["domain"] != "script" || args["service"] != "turn_on" || args["entity_id"] != "script.porridge" || args["wait"] != true || len(args["data"].(map[string]any)) != 0 {
			return nil, errors.New("unsafe service call")
		}
		if f.fail == "ack" {
			return jsontext.Value(`{"success":false}`), nil
		}
		if f.fail == "missing_ack" {
			return jsontext.Value(`{}`), nil
		}
		v = map[string]any{"success": true}
	case "ha_get_state":
		f.reads++
		if f.fail == "readback" && f.actions > 0 {
			return nil, errors.New("readback failed")
		}
		if f.fail == "malformed" {
			return jsontext.Value(`{"data":{}}`), nil
		}
		stamp := f.before
		if f.actions > 0 {
			stamp = f.after
		}
		var last any
		if stamp != "" {
			last = stamp
		}
		state := f.state
		if state == "" {
			state = "off"
		}
		name := "Хочу кашу"
		if f.rename {
			name = "Другое имя"
		}
		id := args["entity_id"]
		if f.fail == "wrong_id" {
			id = "script.other"
		}
		v = map[string]any{"data": map[string]any{"entity_id": id, "state": state, "attributes": map[string]any{"friendly_name": name, "last_triggered": last}}}
	default:
		return nil, fmt.Errorf("unexpected tool %s", tool)
	}
	raw, err := json.Marshal(v)
	return raw, err
}
func TestScriptExactNameAndAssistScope(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int
	}{
		{"Хочу кашу", 1}, {"  ХОЧУ   кашу?! ", 1}, {"Запусти хочу кашу", 1},
		{"Запусти скрипт хочу кашу", 1}, {"Выполни сценарий хочу кашу", 1},
		{"не хочу кашу", 0}, {"я хочу кашу", 0}, {"хочу кашу завтра", 0},
		{"кашу", 0}, {"script.porridge", 0}, {"псевдоним", 0}, {"", 0},
	} {
		t.Run(tc.text, func(t *testing.T) {
			f := &scriptFixture{entities: []Entity{
				{ID: "script.porridge", Name: "Хочу кашу", Aliases: []string{"псевдоним"}},
				{ID: "script.hidden", Name: "Хочу кашу"}, {ID: "switch.fake", Name: "Хочу кашу"},
				{ID: "script.bad,script.other", Name: "Хочу кашу"},
			}, hidden: map[string]bool{"script.hidden": true}}
			c := Controller{Tools: f, Domains: []string{"light"}}
			ids, err := c.FindScripts(t.Context(), tc.text)
			if err != nil || len(ids) != tc.want || (len(ids) > 0 && ids[0] != "script.porridge") || f.actions != 0 {
				t.Fatalf("ids=%v err=%v actions=%d", ids, err, f.actions)
			}
		})
	}
}
func TestScriptAmbiguityIncludesNormalizedAndPrefixedNames(t *testing.T) {
	for _, second := range []string{"ХОЧУ  КАШУ!", "Запусти хочу кашу"} {
		f := &scriptFixture{entities: []Entity{{ID: "script.porridge", Name: "Хочу кашу"}, {ID: "script.other", Name: second}}}
		c := Controller{Tools: f}
		ids, err := c.FindScripts(t.Context(), "Запусти хочу кашу")
		if err != nil || len(ids) != 2 {
			t.Fatalf("ids=%v err=%v", ids, err)
		}
		if _, err := c.RunScript(t.Context(), "Запусти хочу кашу", "script.porridge"); err == nil || f.actions != 0 {
			t.Fatalf("ambiguity executed: %v", err)
		}
	}
}
func TestScriptRunVerifiesNewStartEvenWhenAlreadyFinished(t *testing.T) {
	for _, tc := range []struct {
		name, before, after, state, fail string
		revoke, rename                   bool
		wantActions                      int
		wantErr                          bool
	}{
		{name: "first fast run", after: "2026-11-22T12:00:01Z", wantActions: 1},
		{name: "subsequent fast run", before: "2026-11-22T12:00:00Z", after: "2026-11-22T12:00:01Z", wantActions: 1},
		{name: "long running", after: "2026-11-22T12:00:01Z", state: "on", wantActions: 1},
		{name: "ack only", wantActions: 1, wantErr: true},
		{name: "already running unchanged", before: "2026-11-22T12:00:00Z", after: "2026-11-22T12:00:00Z", state: "on", wantActions: 1, wantErr: true},
		{name: "older timestamp", before: "2026-11-22T12:00:01Z", after: "2026-11-22T12:00:00Z", wantActions: 1, wantErr: true},
		{name: "unavailable", state: "unavailable", wantErr: true},
		{name: "unknown", state: "unknown", wantErr: true},
		{name: "rename", rename: true, wantErr: true},
		{name: "revoked after discovery", revoke: true, wantErr: true},
		{name: "service transport", fail: "ha_call_service", wantErr: true},
		{name: "service negative ack", fail: "ack", wantActions: 1, wantErr: true},
		{name: "service missing ack", fail: "missing_ack", wantActions: 1, wantErr: true},
		{name: "readback transport", fail: "readback", wantActions: 1, wantErr: true},
		{name: "state missing", fail: "malformed", wantErr: true},
		{name: "wrong state target", fail: "wrong_id", wantErr: true},
		{name: "exposure transport", fail: "ha_get_entity_exposure", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &scriptFixture{entities: []Entity{{ID: "script.porridge", Name: "Хочу кашу"}}, before: tc.before, after: tc.after, state: tc.state, fail: tc.fail, revoke: tc.revoke, rename: tc.rename}
			c := Controller{Tools: f}
			ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
			defer cancel()
			answer, err := c.RunScript(ctx, "Хочу кашу", "script.porridge")
			if (err != nil) != tc.wantErr || f.actions != tc.wantActions || (!tc.wantErr && answer != "Запускаю.") || (tc.wantErr && answer != "") {
				t.Fatalf("answer=%q err=%v actions=%d", answer, err, f.actions)
			}
		})
	}
}
func TestScriptRejectsRevokedOrChangedSelection(t *testing.T) {
	f := &scriptFixture{entities: []Entity{{ID: "script.porridge", Name: "Хочу кашу"}}}
	c := Controller{Tools: f}
	if ids, err := c.FindScripts(t.Context(), "Хочу кашу"); err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
	f.hidden = map[string]bool{"script.porridge": true}
	if _, err := c.RunScript(t.Context(), "Хочу кашу", "script.porridge"); err == nil || f.actions != 0 {
		t.Fatal("cached exposure allowed execution", err)
	}
	f.hidden = nil
	f.entities = []Entity{{ID: "script.other", Name: "Хочу кашу"}}
	if _, err := c.RunScript(t.Context(), "Хочу кашу", "script.porridge"); err == nil || f.actions != 0 {
		t.Fatal("stale target executed", err)
	}
}
func TestScriptInventoryMustBeComplete(t *testing.T) {
	for _, raw := range []string{
		`{"entities":[{"entity_id":"script.porridge","friendly_name":"Хочу кашу"}],"partial":true}`,
		`{"entities":[],"has_more":true,"next_offset":0}`,
		`{"entities":[],"has_more":true,"next_offset":2000}`,
	} {
		c := Controller{Tools: toolFunc(func(context.Context, string, map[string]any) (jsontext.Value, error) { return jsontext.Value(raw), nil })}
		if _, err := c.FindScripts(t.Context(), "Хочу кашу"); err == nil {
			t.Fatal("incomplete inventory accepted", raw)
		}
	}
}
func TestScriptPaginationDetectsAmbiguityAcrossPages(t *testing.T) {
	c := Controller{Tools: toolFunc(func(_ context.Context, name string, args map[string]any) (jsontext.Value, error) {
		if name == "ha_get_entity_exposure" {
			return jsontext.Value(`{"exposed_to":{"conversation":true}}`), nil
		}
		if args["offset"] == 0 {
			return jsontext.Value(`{"entities":[{"entity_id":"script.first","friendly_name":"Хочу кашу"}],"has_more":true,"next_offset":200}`), nil
		}
		return jsontext.Value(`{"entities":[{"entity_id":"script.second","friendly_name":"Хочу кашу"}]}`), nil
	})}
	ids, err := c.FindScripts(t.Context(), "Хочу кашу")
	if err != nil || len(ids) != 2 {
		t.Fatal(ids, err)
	}
}
func TestCancelledScriptNeverActuates(t *testing.T) {
	f := &scriptFixture{entities: []Entity{{ID: "script.porridge", Name: "Хочу кашу"}}}
	c := Controller{Tools: f}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.RunScript(ctx, "Хочу кашу", "script.porridge"); !errors.Is(err, context.Canceled) || f.actions != 0 {
		t.Fatal(err, f.actions)
	}
}
