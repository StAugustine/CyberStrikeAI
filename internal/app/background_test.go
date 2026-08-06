package app

import (
	"context"
	"testing"
	"time"
)

func TestBackgroundTaskStopsWithLifecycleContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := startBackgroundTask(ctx, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
	})
	<-started
	cancel()

	waitContext, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := waitForBackgroundTasks(waitContext, []<-chan struct{}{done}); err != nil {
		t.Fatalf("wait for background task: %v", err)
	}
}

func TestAppRejectsBackgroundTasksAfterShutdownStarts(t *testing.T) {
	lifecycleContext, lifecycleCancel := context.WithCancel(context.Background())
	application := &App{lifecycleContext: lifecycleContext, lifecycleCancel: lifecycleCancel}
	started := make(chan struct{})
	if !application.startManagedBackgroundTask(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
	}) {
		t.Fatal("background task should start before shutdown")
	}
	<-started
	done := application.stopBackgroundTasks()
	if application.startManagedBackgroundTask(func(context.Context) {}) {
		t.Fatal("background task started after shutdown")
	}

	waitContext, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := waitForBackgroundTasks(waitContext, done); err != nil {
		t.Fatalf("wait for managed background task: %v", err)
	}
}
