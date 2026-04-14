package pg2json

import (
	"context"
	"sync"
	"time"
)

// attachContext arranges for the *user's* ctx cancellation to:
//  1. send a CancelRequest on a side connection so the server stops the
//     query promptly, and
//  2. force any in-flight Read on this connection to return promptly via
//     a past deadline.
//
// detachContext is the normal-completion path; it stops the watcher
// without firing a cancel.

type ctxState struct {
	stop chan struct{}
	wg   sync.WaitGroup
}

var ctxStates sync.Map // *Client -> *ctxState

func (c *Client) attachContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	done := ctx.Done()
	if done == nil {
		// Background-shaped context with no deadline; nothing to watch.
		return nil
	}
	st := &ctxState{stop: make(chan struct{})}
	st.wg.Add(1)
	go func() {
		defer st.wg.Done()
		select {
		case <-done:
			// Real user cancellation: cancel the server-side query.
			cancelCtx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = c.SendCancel(cancelCtx)
			ccancel()
			_ = c.conn.Net().SetDeadline(time.Unix(1, 0))
		case <-st.stop:
			// Normal completion; do nothing.
		}
	}()
	ctxStates.Store(c, st)
	return nil
}

func (c *Client) detachContext() {
	v, ok := ctxStates.LoadAndDelete(c)
	if !ok {
		return
	}
	st := v.(*ctxState)
	close(st.stop)
	st.wg.Wait()
	_ = c.conn.Net().SetDeadline(time.Time{})
}
