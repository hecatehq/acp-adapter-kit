package commandbridge

import (
	"context"
	"testing"
)

func TestFinalizedPromptRetainsActiveOwnershipUntilItsHandlerEnds(t *testing.T) {
	const sessionID = "session-1"
	bridge := New(Spec{})
	bridge.sessions[sessionID] = &sessionState{Session: Session{ID: sessionID}}

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	first, rpcErr := bridge.beginPrompt(sessionID, cancelFirst)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	finalized := make(chan struct{})
	releaseHandler := make(chan struct{})
	handlerEnded := make(chan struct{})
	go func() {
		_, _, committed := bridge.finalizePromptSuccess(sessionID, first, firstCtx, "question", "answer")
		if !committed {
			t.Error("first prompt was not committed")
		}
		close(finalized)
		<-releaseHandler // Simulate a blocked post-finalize notification/return.
		bridge.endPrompt(sessionID, first)
		close(handlerEnded)
	}()
	<-finalized

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	if second, busyErr := bridge.beginPrompt(sessionID, cancelSecond); second != nil || busyErr == nil || busyErr.Code != -32009 {
		t.Fatalf("begin while finalized handler is active = token %#v, error %#v; want session busy", second, busyErr)
	}
	if bridge.cancel(sessionID) {
		t.Fatal("cancel reported a finalized prompt as cancellable")
	}
	if firstCtx.Err() != nil {
		t.Fatalf("finalized prompt context was cancelled: %v", firstCtx.Err())
	}

	close(releaseHandler)
	<-handlerEnded
	second, rpcErr := bridge.beginPrompt(sessionID, cancelSecond)
	if rpcErr != nil {
		t.Fatalf("begin second prompt after first handler ended: %v", rpcErr)
	}
	bridge.endPrompt(sessionID, first) // A stale deferred end must not steal the new token.
	if !bridge.cancel(sessionID) {
		t.Fatal("second prompt was no longer cancellable after stale first-prompt end")
	}
	if secondCtx.Err() == nil {
		t.Fatal("second prompt context was not cancelled")
	}
	bridge.endPrompt(sessionID, second)

	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.active[sessionID] != nil {
		t.Fatal("active prompt token remains after its own end")
	}
	if got := bridge.sessions[sessionID].PromptCount; got != 1 {
		t.Fatalf("prompt count = %d, want only finalized first prompt", got)
	}
}
