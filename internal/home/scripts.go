package home

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var scriptID = regexp.MustCompile(`^script\.[a-z0-9_]+$`)

// Normalize only case, whitespace and sentence-ending punctuation. Never use
// fuzzy, substring, alias or entity-ID matching for physical actions.
func scriptName(text string) string {
	return strings.Join(strings.Fields(strings.TrimRight(strings.TrimSpace(strings.ToLower(text)), ".!?…")), " ")
}

func scriptNames(text string) []string {
	name := scriptName(text)
	names := []string{name}
	for _, prefix := range []string{"запусти скрипт ", "запусти сценарий ", "выполни скрипт ", "выполни сценарий ", "запусти ", "выполни "} {
		if rest, ok := strings.CutPrefix(name, prefix); ok && rest != "" {
			names = append(names, rest)
			break
		}
	}
	return names
}

// FindScripts reads a complete script inventory and fresh Assist permissions.
// All matches are returned, so ambiguity never silently selects one entity.
func (c *Controller) FindScripts(ctx context.Context, text string) ([]string, error) {
	if scriptName(text) == "" {
		return nil, nil
	}
	inventory := Controller{Tools: c.Tools, Domains: []string{"script"}}
	entities, err := inventory.Entities(ctx)
	if err != nil {
		return nil, fmt.Errorf("script discovery failed: %w", err)
	}
	names := scriptNames(text)
	var ids []string
	for _, entity := range entities {
		if !scriptID.MatchString(entity.ID) || scriptName(entity.Name) == "" {
			continue
		}
		matched := false
		for _, name := range names {
			if scriptName(entity.Name) == name {
				matched = true
			}
		}
		if !matched {
			continue
		}
		visible, err := c.exposed(ctx, entity.ID, false)
		if err != nil {
			return nil, err
		}
		if visible {
			ids = append(ids, entity.ID)
		}
	}
	return ids, nil
}

type scriptState struct {
	ID         string `json:"entity_id"`
	State      string `json:"state"`
	Attributes struct {
		Name          string     `json:"friendly_name"`
		LastTriggered *time.Time `json:"last_triggered"`
	} `json:"attributes"`
}

func (c *Controller) readScript(ctx context.Context, id string) (scriptState, error) {
	raw, err := c.Tools.Call(ctx, "ha_get_state", map[string]any{"entity_id": id, "fields": []string{"entity_id", "state", "attributes"}})
	if err != nil {
		return scriptState{}, err
	}
	var result struct {
		Data    scriptState `json:"data"`
		Success *bool       `json:"success"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return scriptState{}, err
	}
	if (result.Success != nil && !*result.Success) || result.Data.ID != id || (result.Data.State != "on" && result.Data.State != "off") {
		return scriptState{}, errors.New("script state is missing or unavailable")
	}
	return result.Data, nil
}

// RunScript confirms a new start, not completion of the script's actions.
// Never retry a service call: even a transport failure may follow execution.
func (c *Controller) RunScript(ctx context.Context, text, id string) (string, error) {
	if !scriptID.MatchString(id) {
		return "", errors.New("invalid script target")
	}
	ids, err := c.FindScripts(ctx, text)
	if err != nil {
		return "", err
	}
	if len(ids) != 1 || ids[0] != id {
		return "", errors.New("script selection changed or became ambiguous")
	}
	before, err := c.readScript(ctx, id)
	if err != nil {
		return "", err
	}
	// Verify the current friendly name, in case discovery raced with a rename.
	matched := false
	for _, name := range scriptNames(text) {
		if scriptName(before.Attributes.Name) == name {
			matched = true
		}
	}
	if !matched {
		return "", errors.New("script friendly name changed")
	}
	visible, err := c.exposed(ctx, id, false)
	if err != nil {
		return "", err
	}
	if !visible {
		return "", errors.New("script is not exposed to Assist")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	raw, err := c.Tools.Call(ctx, "ha_call_service", map[string]any{"domain": "script", "service": "turn_on", "entity_id": id, "data": map[string]any{}, "wait": true})
	if err != nil {
		return "", fmt.Errorf("script start failed; no cloud retry: %w", err)
	}
	var outcome struct {
		Success *bool `json:"success"`
	}
	if json.Unmarshal(raw, &outcome) != nil || outcome.Success == nil || !*outcome.Success {
		return "", errors.New("script start was not acknowledged; no retry")
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for {
		after, err := c.readScript(verifyCtx, id)
		if err != nil {
			return "", fmt.Errorf("script start readback failed; no retry: %w", err)
		}
		triggered := after.Attributes.LastTriggered
		if triggered != nil && (before.Attributes.LastTriggered == nil || triggered.After(*before.Attributes.LastTriggered)) {
			return "Запускаю.", nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-verifyCtx.Done():
			timer.Stop()
			return "", fmt.Errorf("script start was not confirmed; no retry: %w", verifyCtx.Err())
		case <-timer.C:
		}
	}
}
