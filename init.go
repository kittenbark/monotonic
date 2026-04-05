package mono

import (
	"sync/atomic"
)

var (
	Port atomic.Int64
)

func init() {
	Port.Store(7070)
}
