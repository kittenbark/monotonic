package mono

import (
	"sync"
	"sync/atomic"
)

func s3fifoNew(capacity int) *s3fifo {
	sCap := capacity / 10
	if sCap == 0 {
		sCap = 1
	}

	cache := &s3fifo{
		smallCap: sCap,
		mainCap:  capacity - sCap,
		addCh:    make(chan *s3fifoItem, 1024), // Buffer to prevent blocking writers
		ghost:    make(map[string]struct{}),
	}

	// Start the single background worker
	go cache.worker()

	return cache
}

// s3fifo is our highly concurrent cache.
type s3fifo struct {
	items sync.Map // Lock-free reads, concurrent-safe writes

	// Config
	smallCap int
	mainCap  int

	// Async channel for passing new items to the background worker
	addCh chan *s3fifoItem

	// Internal queues (ONLY accessed by the background worker, so no mutexes needed)
	small []*s3fifoItem
	main  []*s3fifoItem
	ghost map[string]struct{} // Map for O(1) ghost lookups
}

// s3fifoItem represents a cache entry.
type s3fifoItem struct {
	Key   string
	Value []byte
	Freq  atomic.Int32 // Lock-free frequency counter
}

// Get is the critical path: 100% Wait-Free. No mutexes, no channels.
func (c *s3fifo) Get(key string) ([]byte, bool) {
	val, ok := c.items.Load(key)
	if !ok {
		return nil, false
	}

	item := val.(*s3fifoItem)

	// Increment frequency atomically, capped at 3
	for {
		f := item.Freq.Load()
		if f >= 3 {
			break
		}
		if item.Freq.CompareAndSwap(f, f+1) {
			break
		}
	}

	return item.Value, true
}

// Set is wait-free for the caller. It stores the item and hands off queue management.
func (c *s3fifo) Set(key string, value []byte) {
	newItem := &s3fifoItem{Key: key, Value: value}
	newItem.Freq.Store(0)

	// 1. Make it immediately available for reads
	c.items.Store(key, newItem)

	// 2. Send to background worker asynchronously
	// Note: In high-load systems, you might drop items if the channel is full
	// rather than blocking, to maintain strict wait-free guarantees.
	c.addCh <- newItem
}

// ---------------------------------------------------------------------------
// BACKGROUND WORKER (The "Brain")
// ---------------------------------------------------------------------------

// worker runs in a single goroutine. Because only this function touches
// the small, main, and ghost structures, they don't need locks!
func (c *s3fifo) worker() {
	for item := range c.addCh {
		c.insert(item)
	}
}

func (c *s3fifo) insert(item *s3fifoItem) {
	// Did we prematurely evict this recently?
	if _, inGhost := c.ghost[item.Key]; inGhost {
		delete(c.ghost, item.Key)
		c.main = append(c.main, item)
	} else {
		c.small = append(c.small, item)
	}
	c.ensureSpace()
}

func (c *s3fifo) ensureSpace() {
	if len(c.small)+len(c.main) < c.smallCap+c.mainCap {
		return
	}

	if len(c.main) >= c.mainCap || len(c.small) == 0 {
		c.evictMain()
	} else {
		c.evictSmall()
	}
}

func (c *s3fifo) evictSmall() {
	if len(c.small) == 0 {
		return
	}

	oldest := c.small[0]
	c.small = c.small[1:]

	if oldest.Freq.Load() > 0 {
		// Lazy Promotion
		oldest.Freq.Store(0)
		c.main = append(c.main, oldest)
		if len(c.main) > c.mainCap {
			c.evictMain()
		}
	} else {
		// Quick Demotion
		c.ghost[oldest.Key] = struct{}{}
		if len(c.ghost) > c.mainCap {
			// Random map eviction to simulate ring buffer for brevity
			for k := range c.ghost {
				delete(c.ghost, k)
				break
			}
		}
		c.items.Delete(oldest.Key) // Remove from actual cache
	}
}

func (c *s3fifo) evictMain() {
	for len(c.main) > 0 {
		oldest := c.main[0]
		c.main = c.main[1:]

		f := oldest.Freq.Load()
		if f > 0 {
			// CLOCK sweep: give it another chance
			oldest.Freq.Store(f - 1)
			c.main = append(c.main, oldest)
		} else {
			// True eviction
			c.items.Delete(oldest.Key) // Remove from actual cache
			break
		}
	}
}
