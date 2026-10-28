package api

import (
	"context"
	"github.com/muse2api/muse2api/internal/store"
	"log"
	"time"
)

// startTask owns the lifetime and durable state transitions for admitted media jobs.
func (s *Server) startTask(task *store.Task, prompt string, refs []string, timeout time.Duration, startProgress int) {
	go func() {
		ctx, cancel := context.WithTimeout(s.jobCtx, timeout)
		defer cancel()
		started := time.Now()
		var saveErr error
		update := func(apply func(*store.Task)) {
			if saveErr != nil {
				return
			}
			_, saveErr = s.store.UpdateTask(task.ID, apply)
			if saveErr != nil {
				cancel()
			}
		}
		onAcquired := func() {
			started = time.Now()
			update(func(t *store.Task) { t.Status = "processing"; t.Progress = startProgress })
		}
		onProgress := func(p int) { update(func(t *store.Task) { t.Progress = p }) }
		res, accountID, err := s.RunGeneration(ctx, prompt, task.Kind, timeout, "", onProgress, refs, onAcquired)
		if saveErr != nil {
			err = saveErr
		}
		_, writeErr := s.store.UpdateTask(task.ID, func(t *store.Task) {
			t.Elapsed = roundOne(time.Since(started).Seconds())
			if err != nil {
				t.Status = "failed"
				t.Error = err.Error()
				return
			}
			url := s.cfg.MediaURL(res.Filename)
			t.Status = "completed"
			t.Progress = 100
			t.Account = accountID
			t.URL = url
			t.Result = &store.GenerationResult{URL: url, Filename: res.Filename, Size: res.Size, Bytes: res.Size, Kind: res.Kind}
			if task.Kind == "video" {
				t.Video = map[string]string{"url": url}
			}
		})
		if writeErr != nil {
			log.Printf("task %s state could not be saved: %v", task.ID, writeErr)
		}
	}()
}
