package app

import "context"

func startBackgroundTask(ctx context.Context, task func(context.Context)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		task(ctx)
	}()
	return done
}

func waitForBackgroundTasks(ctx context.Context, tasks []<-chan struct{}) error {
	for _, task := range tasks {
		if task == nil {
			continue
		}
		select {
		case <-task:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (a *App) startManagedBackgroundTask(task func(context.Context)) bool {
	if a == nil || task == nil {
		return false
	}
	a.backgroundMu.Lock()
	defer a.backgroundMu.Unlock()
	if a.backgroundClosed || a.lifecycleContext == nil {
		return false
	}
	a.backgroundDone = append(a.backgroundDone, startBackgroundTask(a.lifecycleContext, task))
	return true
}

func (a *App) stopBackgroundTasks() []<-chan struct{} {
	a.backgroundMu.Lock()
	defer a.backgroundMu.Unlock()
	a.backgroundClosed = true
	if a.lifecycleCancel != nil {
		a.lifecycleCancel()
	}
	return append([]<-chan struct{}(nil), a.backgroundDone...)
}
