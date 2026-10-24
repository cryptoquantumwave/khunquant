package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/media"
)

func originCtx(origin, channel, chatID string) context.Context {
	ctx := WithToolOrigin(context.Background(), origin)
	return WithToolContext(ctx, channel, chatID)
}

func newRecordingMessageTool() (*MessageTool, *[][3]string) {
	var sent [][3]string
	tool := NewMessageTool()
	tool.SetSendCallback(func(channel, chatID, content string) error {
		sent = append(sent, [3]string{channel, chatID, content})
		return nil
	})
	return tool, &sent
}

func TestMessageTool_AutonomousTurnMayOnlyMessageItsOwnChat(t *testing.T) {
	for _, origin := range []string{OriginHeartbeat, OriginCron} {
		t.Run(origin+"/no explicit target sends to own route", func(t *testing.T) {
			tool, sent := newRecordingMessageTool()
			res := tool.Execute(originCtx(origin, "telegram", "owner-1"), map[string]any{"content": "hi"})
			if res.IsError {
				t.Fatalf("own route refused: %s", res.ForLLM)
			}
			if len(*sent) != 1 || (*sent)[0] != [3]string{"telegram", "owner-1", "hi"} {
				t.Fatalf("sent = %v, want one send to telegram:owner-1", *sent)
			}
		})

		t.Run(origin+"/explicit own route is allowed", func(t *testing.T) {
			tool, sent := newRecordingMessageTool()
			res := tool.Execute(originCtx(origin, "telegram", "owner-1"), map[string]any{
				"content": "hi", "channel": "telegram", "chat_id": "owner-1",
			})
			if res.IsError || len(*sent) != 1 {
				t.Fatalf("own route refused: err=%v sent=%v", res.ForLLM, *sent)
			}
		})

		for name, args := range map[string]map[string]any{
			"other chat_id":          {"content": "balances", "chat_id": "attacker"},
			"other channel":          {"content": "balances", "channel": "discord"},
			"other channel and chat": {"content": "balances", "channel": "discord", "chat_id": "attacker"},
			"own channel other chat": {"content": "balances", "channel": "telegram", "chat_id": "attacker"},
		} {
			t.Run(origin+"/refuses "+name, func(t *testing.T) {
				tool, sent := newRecordingMessageTool()
				res := tool.Execute(originCtx(origin, "telegram", "owner-1"), args)
				if !res.IsError {
					t.Fatal("expected a refusal")
				}
				if !strings.Contains(res.ForLLM, "an autonomous turn may only message its own chat") {
					t.Fatalf("refusal = %q", res.ForLLM)
				}
				if len(*sent) != 0 {
					t.Fatalf("callback called despite refusal: %v", *sent)
				}
			})
		}
	}
}

func TestMessageTool_AutonomousTurnWithoutOwnRouteCannotChooseOne(t *testing.T) {
	tool, sent := newRecordingMessageTool()
	res := tool.Execute(originCtx(OriginHeartbeat, "", ""), map[string]any{
		"content": "x", "channel": "telegram", "chat_id": "attacker",
	})
	if !res.IsError || len(*sent) != 0 {
		t.Fatalf("expected refusal with no send, got err=%v sent=%v", res.IsError, *sent)
	}
}

func TestMessageTool_PersonTurnMayMessageAnotherChat(t *testing.T) {
	for _, origin := range []string{OriginRemote, OriginInternal} {
		t.Run(origin, func(t *testing.T) {
			tool, sent := newRecordingMessageTool()
			res := tool.Execute(originCtx(origin, "telegram", "owner-1"), map[string]any{
				"content": "hi", "channel": "discord", "chat_id": "other",
			})
			if res.IsError {
				t.Fatalf("person turn refused: %s", res.ForLLM)
			}
			if len(*sent) != 1 || (*sent)[0] != [3]string{"discord", "other", "hi"} {
				t.Fatalf("sent = %v", *sent)
			}
		})
	}
}

func newSendFileFixture(t *testing.T) (*SendFileTool, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(file, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewSendFileTool(dir, false, 1024, media.NewFileMediaStore()), file
}

func TestSendFileTool_AutonomousTurnMayOnlySendToItsOwnChat(t *testing.T) {
	for _, origin := range []string{OriginHeartbeat, OriginCron} {
		t.Run(origin+"/no explicit target sends to own route", func(t *testing.T) {
			tool, file := newSendFileFixture(t)
			res := tool.Execute(originCtx(origin, "telegram", "owner-1"), map[string]any{"path": file})
			if res.IsError {
				t.Fatalf("own route refused: %s", res.ForLLM)
			}
			if len(res.Media) != 1 {
				t.Fatalf("media = %v, want one ref", res.Media)
			}
		})

		for name, args := range map[string]map[string]any{
			"other chat_id": {"chat_id": "attacker"},
			"other channel": {"channel": "discord"},
		} {
			t.Run(origin+"/refuses "+name, func(t *testing.T) {
				tool, file := newSendFileFixture(t)
				args["path"] = file
				res := tool.Execute(originCtx(origin, "telegram", "owner-1"), args)
				if !res.IsError {
					t.Fatal("expected a refusal")
				}
				if !strings.Contains(res.ForLLM, "an autonomous turn may only message its own chat") {
					t.Fatalf("refusal = %q", res.ForLLM)
				}
				if len(res.Media) != 0 {
					t.Fatalf("media registered despite refusal: %v", res.Media)
				}
			})
		}
	}
}

func TestSendFileTool_AutonomousTurnIgnoresSetContextFallback(t *testing.T) {
	tool, file := newSendFileFixture(t)
	tool.SetContext("telegram", "stale-chat")
	res := tool.Execute(originCtx(OriginHeartbeat, "", ""), map[string]any{"path": file})
	if !res.IsError {
		t.Fatal("an autonomous turn without its own route must not fall back to the stored default")
	}
}

func TestSendFileTool_PersonTurnUnchanged(t *testing.T) {
	tool, file := newSendFileFixture(t)
	res := tool.Execute(originCtx(OriginRemote, "telegram", "owner-1"), map[string]any{
		"path": file, "chat_id": "other",
	})
	if res.IsError {
		t.Fatalf("person turn refused: %s", res.ForLLM)
	}
}
