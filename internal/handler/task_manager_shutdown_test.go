package handler

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAgentTaskManagerShutdownCancelsWorkAndStopsNewTasks(t *testing.T) {
	manager := NewAgentTaskManager()
	taskContext, cancelTask := context.WithCancelCause(context.Background())
	if _, err := manager.StartTask("conversation-1", "test", cancelTask); err != nil {
		t.Fatalf("start task: %v", err)
	}
	executeContext, cancelExecute := context.WithCancel(context.Background())
	manager.RegisterActiveEinoExecute("conversation-1", cancelExecute)
	toolCancelled := make(chan string, 1)
	manager.SetToolCanceler(func(conversationID string) { toolCancelled <- conversationID })

	shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := manager.Shutdown(shutdownContext); err != nil {
		t.Fatalf("shutdown task manager: %v", err)
	}
	if err := manager.Shutdown(shutdownContext); err != nil {
		t.Fatalf("repeat shutdown task manager: %v", err)
	}
	if !errors.Is(context.Cause(taskContext), ErrTaskCancelled) {
		t.Fatalf("task cancellation cause = %v, want ErrTaskCancelled", context.Cause(taskContext))
	}
	select {
	case <-executeContext.Done():
	default:
		t.Fatal("active Eino execute was not cancelled")
	}
	select {
	case conversationID := <-toolCancelled:
		if conversationID != "conversation-1" {
			t.Fatalf("tool cancellation conversation = %q", conversationID)
		}
	default:
		t.Fatal("active MCP tools were not cancelled")
	}
	if _, err := manager.StartTask("conversation-2", "late", nil); !errors.Is(err, ErrTaskCancelled) {
		t.Fatalf("task after shutdown error = %v, want ErrTaskCancelled", err)
	}
}
