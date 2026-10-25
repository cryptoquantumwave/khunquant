package tools

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestMessageTool_Execute_Success(t *testing.T) {
	tool := NewMessageTool()

	var sentChannel, sentChatID, sentContent string
	tool.SetSendCallback(func(channel, chatID, content string) error {
		sentChannel = channel
		sentChatID = chatID
		sentContent = content
		return nil
	})

	ctx := WithToolContext(context.Background(), "test-channel", "test-chat-id")
	args := map[string]any{
		"content": "Hello, world!",
	}

	result := tool.Execute(ctx, args)

	// Verify message was sent with correct parameters
	if sentChannel != "test-channel" {
		t.Errorf("Expected channel 'test-channel', got '%s'", sentChannel)
	}
	if sentChatID != "test-chat-id" {
		t.Errorf("Expected chatID 'test-chat-id', got '%s'", sentChatID)
	}
	if sentContent != "Hello, world!" {
		t.Errorf("Expected content 'Hello, world!', got '%s'", sentContent)
	}

	// Verify ToolResult meets US-011 criteria:
	// - Send success returns SilentResult (Silent=true)
	if !result.Silent {
		t.Error("Expected Silent=true for successful send")
	}

	// - ForLLM contains send status description
	if result.ForLLM != "Message sent to test-channel:test-chat-id" {
		t.Errorf("Expected ForLLM 'Message sent to test-channel:test-chat-id', got '%s'", result.ForLLM)
	}

	// - ForUser is empty (user already received message directly)
	if result.ForUser != "" {
		t.Errorf("Expected ForUser to be empty, got '%s'", result.ForUser)
	}

	// - IsError should be false
	if result.IsError {
		t.Error("Expected IsError=false for successful send")
	}
}

func TestMessageTool_Execute_WithCustomChannel(t *testing.T) {
	tool := NewMessageTool()

	var sentChannel, sentChatID string
	tool.SetSendCallback(func(channel, chatID, content string) error {
		sentChannel = channel
		sentChatID = chatID
		return nil
	})

	ctx := WithToolContext(context.Background(), "default-channel", "default-chat-id")
	args := map[string]any{
		"content": "Test message",
		"channel": "custom-channel",
		"chat_id": "custom-chat-id",
	}

	result := tool.Execute(ctx, args)

	// Verify custom channel/chatID were used instead of defaults
	if sentChannel != "custom-channel" {
		t.Errorf("Expected channel 'custom-channel', got '%s'", sentChannel)
	}
	if sentChatID != "custom-chat-id" {
		t.Errorf("Expected chatID 'custom-chat-id', got '%s'", sentChatID)
	}

	if !result.Silent {
		t.Error("Expected Silent=true")
	}
	if result.ForLLM != "Message sent to custom-channel:custom-chat-id" {
		t.Errorf("Expected ForLLM 'Message sent to custom-channel:custom-chat-id', got '%s'", result.ForLLM)
	}
}

func TestMessageTool_Execute_SendFailure(t *testing.T) {
	tool := NewMessageTool()

	sendErr := errors.New("network error")
	tool.SetSendCallback(func(channel, chatID, content string) error {
		return sendErr
	})

	ctx := WithToolContext(context.Background(), "test-channel", "test-chat-id")
	args := map[string]any{
		"content": "Test message",
	}

	result := tool.Execute(ctx, args)

	// Verify ToolResult for send failure:
	// - Send failure returns ErrorResult (IsError=true)
	if !result.IsError {
		t.Error("Expected IsError=true for failed send")
	}

	// - ForLLM contains error description
	expectedErrMsg := "sending message: network error"
	if result.ForLLM != expectedErrMsg {
		t.Errorf("Expected ForLLM '%s', got '%s'", expectedErrMsg, result.ForLLM)
	}

	// - Err field should contain original error
	if result.Err == nil {
		t.Error("Expected Err to be set")
	}
	if result.Err != sendErr {
		t.Errorf("Expected Err to be sendErr, got %v", result.Err)
	}
}

func TestMessageTool_Execute_MissingContent(t *testing.T) {
	tool := NewMessageTool()

	ctx := WithToolContext(context.Background(), "test-channel", "test-chat-id")
	args := map[string]any{} // content missing

	result := tool.Execute(ctx, args)

	// Verify error result for missing content
	if !result.IsError {
		t.Error("Expected IsError=true for missing content")
	}
	if result.ForLLM != "content is required" {
		t.Errorf("Expected ForLLM 'content is required', got '%s'", result.ForLLM)
	}
}

func TestMessageTool_Execute_NoTargetChannel(t *testing.T) {
	tool := NewMessageTool()
	// No WithToolContext — channel/chatID are empty

	tool.SetSendCallback(func(channel, chatID, content string) error {
		return nil
	})

	ctx := context.Background()
	args := map[string]any{
		"content": "Test message",
	}

	result := tool.Execute(ctx, args)

	// Verify error when no target channel specified
	if !result.IsError {
		t.Error("Expected IsError=true when no target channel")
	}
	if result.ForLLM != "No target channel/chat specified" {
		t.Errorf("Expected ForLLM 'No target channel/chat specified', got '%s'", result.ForLLM)
	}
}

func TestMessageTool_Execute_NotConfigured(t *testing.T) {
	tool := NewMessageTool()
	// No SetSendCallback called

	ctx := WithToolContext(context.Background(), "test-channel", "test-chat-id")
	args := map[string]any{
		"content": "Test message",
	}

	result := tool.Execute(ctx, args)

	// Verify error when send callback not configured
	if !result.IsError {
		t.Error("Expected IsError=true when send callback not configured")
	}
	if result.ForLLM != "Message sending not configured" {
		t.Errorf("Expected ForLLM 'Message sending not configured', got '%s'", result.ForLLM)
	}
}

func TestMessageTool_Name(t *testing.T) {
	tool := NewMessageTool()
	if tool.Name() != "message" {
		t.Errorf("Expected name 'message', got '%s'", tool.Name())
	}
}

func TestMessageTool_Description(t *testing.T) {
	tool := NewMessageTool()
	desc := tool.Description()
	if desc == "" {
		t.Error("Description should not be empty")
	}
}

func TestMessageTool_Parameters(t *testing.T) {
	tool := NewMessageTool()
	params := tool.Parameters()

	// Verify parameters structure
	typ, ok := params["type"].(string)
	if !ok || typ != "object" {
		t.Error("Expected type 'object'")
	}

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("Expected properties to be a map")
	}

	// Check required properties
	required, ok := params["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "content" {
		t.Error("Expected 'content' to be required")
	}

	// Check content property
	contentProp, ok := props["content"].(map[string]any)
	if !ok {
		t.Error("Expected 'content' property")
	}
	if contentProp["type"] != "string" {
		t.Error("Expected content type to be 'string'")
	}

	// Check channel property (optional)
	channelProp, ok := props["channel"].(map[string]any)
	if !ok {
		t.Error("Expected 'channel' property")
	}
	if channelProp["type"] != "string" {
		t.Error("Expected channel type to be 'string'")
	}

	// Check chat_id property (optional)
	chatIDProp, ok := props["chat_id"].(map[string]any)
	if !ok {
		t.Error("Expected 'chat_id' property")
	}
	if chatIDProp["type"] != "string" {
		t.Error("Expected chat_id type to be 'string'")
	}
}

// newSendingMessageTool returns a MessageTool whose sends always succeed.
func newSendingMessageTool() *MessageTool {
	mt := NewMessageTool()
	mt.SetSendCallback(func(_, _, _ string) error { return nil })
	return mt
}

// sendInTurn runs the message tool as the agent loop would for a turn: the
// turn ID travels in the context, the target as channel/chat.
func sendInTurn(t *testing.T, mt *MessageTool, turnID, channel, chatID string) {
	t.Helper()
	ctx := WithToolContext(WithTurnID(context.Background(), turnID), channel, chatID)
	if res := mt.Execute(ctx, map[string]any{"content": "hi"}); res.IsError {
		t.Fatalf("message send in turn %q failed: %s", turnID, res.ForLLM)
	}
}

func TestMessageTool_HasSentInTurn_Empty(t *testing.T) {
	mt := NewMessageTool()
	if mt.HasSentInTurn("turn-1") {
		t.Error("new MessageTool should have HasSentInTurn=false")
	}
	if mt.HasSentToInTurn("turn-1", "chan", "chat") {
		t.Error("HasSentToInTurn on empty tool should be false")
	}
	mt.ClearTurn("turn-1") // clearing a turn that never sent must not panic
}

func TestMessageTool_SentStateIsPerTurn(t *testing.T) {
	mt := newSendingMessageTool()

	sendInTurn(t, mt, "turn-a", "telegram", "42")

	if !mt.HasSentInTurn("turn-a") {
		t.Error("turn-a sent a message; HasSentInTurn should be true")
	}
	if !mt.HasSentToInTurn("turn-a", "telegram", "42") {
		t.Error("HasSentToInTurn should match the exact channel+chatID of the sending turn")
	}

	// The whole point: another turn, running at the same time on the same tool
	// instance, sent nothing - and must not be told that it did.
	if mt.HasSentInTurn("turn-b") {
		t.Error("turn-b sent nothing; turn-a's message leaked into it")
	}
	if mt.HasSentToInTurn("turn-b", "telegram", "42") {
		t.Error("turn-b did not send to telegram:42; turn-a's message leaked into it")
	}
}

func TestMessageTool_HasSentToInTurn_NoMatch(t *testing.T) {
	mt := newSendingMessageTool()
	sendInTurn(t, mt, "turn-a", "telegram", "42")

	if mt.HasSentToInTurn("turn-a", "telegram", "99") {
		t.Error("HasSentToInTurn should not match different chatID")
	}
	if mt.HasSentToInTurn("turn-a", "discord", "42") {
		t.Error("HasSentToInTurn should not match different channel")
	}
}

func TestMessageTool_ClearTurn_OnlyClearsThatTurn(t *testing.T) {
	mt := newSendingMessageTool()
	sendInTurn(t, mt, "turn-a", "telegram", "42")
	sendInTurn(t, mt, "turn-b", "telegram", "42")

	mt.ClearTurn("turn-a")

	if mt.HasSentInTurn("turn-a") {
		t.Error("turn-a should be forgotten after ClearTurn")
	}
	if !mt.HasSentToInTurn("turn-b", "telegram", "42") {
		t.Error("clearing turn-a wiped turn-b's state")
	}

	mt.ClearTurn("turn-b")
	mt.mu.Lock()
	defer mt.mu.Unlock()
	if len(mt.sentByTurn) != 0 || len(mt.turnOrder) != 0 {
		t.Errorf("after clearing every turn: %d entries, %d order slots; want 0, 0",
			len(mt.sentByTurn), len(mt.turnOrder))
	}
}

// A send outside any turn has nobody to ask about it and nothing to clear it,
// so it must not be remembered at all.
func TestMessageTool_SendWithoutTurnIsNotRecorded(t *testing.T) {
	mt := newSendingMessageTool()
	ctx := WithToolContext(context.Background(), "telegram", "42")
	if res := mt.Execute(ctx, map[string]any{"content": "hi"}); res.IsError {
		t.Fatalf("send failed: %s", res.ForLLM)
	}
	if mt.HasSentInTurn("") || mt.HasSentToInTurn("", "telegram", "42") {
		t.Error("a send with no turn ID was recorded")
	}
	mt.mu.Lock()
	defer mt.mu.Unlock()
	if len(mt.sentByTurn) != 0 {
		t.Errorf("a send with no turn ID left %d entries behind", len(mt.sentByTurn))
	}
}

// A failed send reached nobody, so it must not suppress the turn's reply.
func TestMessageTool_FailedSendIsNotRecorded(t *testing.T) {
	mt := NewMessageTool()
	mt.SetSendCallback(func(_, _, _ string) error { return errors.New("network down") })

	ctx := WithToolContext(WithTurnID(context.Background(), "turn-a"), "telegram", "42")
	if res := mt.Execute(ctx, map[string]any{"content": "hi"}); !res.IsError {
		t.Fatal("expected the send to fail")
	}
	if mt.HasSentInTurn("turn-a") {
		t.Error("a failed send was recorded as sent")
	}
}

// Turns whose state is never cleared (an async subagent sending after its
// parent finished, a cron turn that failed before its caller could clear) must
// not accumulate without bound.
func TestMessageTool_AbandonedTurnsAreEvictedOldestFirst(t *testing.T) {
	mt := newSendingMessageTool()

	const extra = 10
	for i := 0; i < maxTrackedTurns+extra; i++ {
		sendInTurn(t, mt, fmt.Sprintf("turn-%d", i), "telegram", "42")
	}

	mt.mu.Lock()
	entries, order := len(mt.sentByTurn), len(mt.turnOrder)
	mt.mu.Unlock()
	if entries != maxTrackedTurns || order != maxTrackedTurns {
		t.Fatalf("tracked %d entries / %d order slots, want both capped at %d", entries, order, maxTrackedTurns)
	}

	// The oldest went first; the newest are all still there.
	for i := 0; i < extra; i++ {
		if mt.HasSentInTurn(fmt.Sprintf("turn-%d", i)) {
			t.Errorf("turn-%d is among the oldest and should have been evicted", i)
		}
	}
	for i := extra; i < maxTrackedTurns+extra; i++ {
		if !mt.HasSentInTurn(fmt.Sprintf("turn-%d", i)) {
			t.Fatalf("turn-%d should still be tracked", i)
		}
	}

	// Sending again in a turn that is already tracked evicts nothing.
	sendInTurn(t, mt, fmt.Sprintf("turn-%d", extra), "discord", "7")
	if !mt.HasSentInTurn(fmt.Sprintf("turn-%d", extra+1)) {
		t.Error("a second send in a tracked turn evicted another turn")
	}
}

// One MessageTool serves every turn of an agent. Concurrent turns sending,
// asking and clearing must not race (run with -race).
func TestMessageTool_ConcurrentTurns(t *testing.T) {
	mt := newSendingMessageTool()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			turnID := fmt.Sprintf("turn-%d", i)
			chat := fmt.Sprintf("chat-%d", i)
			ctx := WithToolContext(WithTurnID(context.Background(), turnID), "telegram", chat)
			for j := 0; j < 50; j++ {
				if res := mt.Execute(ctx, map[string]any{"content": "hi"}); res.IsError {
					t.Errorf("%s: send failed: %s", turnID, res.ForLLM)
					return
				}
				if !mt.HasSentToInTurn(turnID, "telegram", chat) {
					t.Errorf("%s: own send not visible", turnID)
					return
				}
				mt.ClearTurn(turnID)
			}
		}(i)
	}
	wg.Wait()
}
