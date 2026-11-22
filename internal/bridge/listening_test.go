package bridge

import (
	"context"
	app "github.com/freequasar/quasar-go-genproto/proto/alice/library/client/protos"
	common "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
	"testing"
	"time"

	"github.com/coder/websocket"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	"quasarbridge/internal/wire"
)

func TestReplySerializesListeningIntentForNormalAndStreamingAnswers(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, listen := range []bool{false, true} {
			var frames []wire.Frame
			n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error {
				frames = append(frames, wire.Decode(b, wire.ServerToClient))
				return nil
			}, nil)
			p := &nativeRequest{id: "request", ctx: t.Context(), request: &request.TAliceRequestProto{Header: &request.TAliceRequestProto_THeader{RequestId: new("app-request"), DialogId: new("dialog")}}, ttsID: "tts", responseID: "response", streaming: streaming, progressAudioDone: make(chan struct{})}
			close(p.progressAudioDone)
			if streaming {
				if err := n.beginDeferred(p); err != nil {
					t.Fatal(err)
				}
				// Even a caller passing true must not open the mic during a keepalive.
				if err := n.deferredPart(p, "", false, true); err != nil {
					t.Fatal(err)
				}
			}
			if err := n.reply(p, "В какой комнате?", listen); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range frames {
				if r := f.Server.GetEvent().GetAliceResponse(); r != nil {
					want := listen && !streaming
					if r.GetVoiceResponse().GetShouldListen() != want {
						t.Fatal("incorrect ordinary response listening flag", streaming, listen)
					}
					if !streaming {
						found = true
					}
				}
				if r := f.Server.GetEvent().GetDeferredAliceResponse(); r != nil {
					if r.GetBaseResponse().GetShouldListen() != (listen && r.GetIsLast()) {
						t.Fatal("incorrect deferred listening flag", listen, r.GetIsLast())
					}
					if r.GetIsLast() {
						found = true
					}
				}
			}
			n.Close()
			if !found {
				t.Fatal("no final response")
			}
		}
	}
}

func TestNativeQuestionAndSpokenClarificationUseSameSpeakerContext(t *testing.T) {
	calls := 0
	e := &Engine{Home: dialogueHomeFunc(func(_ context.Context, text string, history []llm.Message) (dialogue.Reply, error) {
		calls++
		if calls == 1 {
			return dialogue.Reply{Answer: "В какой комнате?", ShouldListen: true}, nil
		}
		if text != "в спальне" || len(history) != 2 || history[0].Content != "Включи свет" {
			t.Error("clarification lost original request", text, history)
		}
		return dialogue.Reply{Answer: "Готово."}, nil
	})}
	for i, text := range []string{"Включи свет", "в спальне"} {
		replies := make(chan bool, 1)
		n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error {
			if r := wire.Decode(b, wire.ServerToClient).Server.GetEvent().GetAliceResponse(); r != nil {
				replies <- r.GetVoiceResponse().GetShouldListen()
			}
			return nil
		}, e)
		req := &request.TAliceRequestProto{Application: &app.TClientInfoProto{DeviceId: new("speaker-a")}, Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new(text)}}}
		input := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: newID()}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: req}}}}
		if err := n.Client(t.Context(), websocket.MessageBinary, aa(input)); err != nil {
			t.Fatal(err)
		}
		select {
		case listen := <-replies:
			if listen != (i == 0) {
				t.Error("wrong listening flag", i, listen)
			}
		case <-time.After(time.Second):
			t.Error("missing reply")
		}
		n.Close()
	}
	if calls != 2 {
		t.Fatal("clarification was not delivered", calls)
	}
}
