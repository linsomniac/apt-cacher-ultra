package acmedns

import (
	"bufio"
	"context"
	"io"
	"strconv"
	"sync"
	"time"
)

// Cancellation uses a separate pipe so it is serviced while an SDK call blocks.
// Monotonic request IDs prevent a delayed cancellation from cancelling cleanup.
type workerControl struct {
	mu                sync.Mutex
	active, cancelled uint64
	cancel            context.CancelFunc
}

func (c *workerControl) begin(id uint64, deadline time.Time) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	c.mu.Lock()
	c.active, c.cancel = id, cancel
	if id <= c.cancelled {
		cancel()
	}
	c.mu.Unlock()
	return ctx, func() {
		c.mu.Lock()
		if c.active == id {
			c.cancel = nil
		}
		c.mu.Unlock()
		cancel()
	}
}

func (c *workerControl) interrupt(id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id > c.cancelled {
		c.cancelled = id
	}
	if c.cancel != nil && c.active <= c.cancelled {
		c.cancel()
	}
}

func (c *workerControl) read(input io.Reader) {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 32), 32)
	for scanner.Scan() {
		id, err := strconv.ParseUint(scanner.Text(), 10, 64)
		if err != nil || id == 0 {
			return
		}
		c.interrupt(id)
	}
}
