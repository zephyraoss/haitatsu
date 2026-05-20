package ids

import (
	"crypto/rand"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

type Generator struct {
	mu      sync.Mutex
	entropy *ulid.MonotonicEntropy
}

func NewGenerator() *Generator {
	return &Generator{entropy: ulid.Monotonic(rand.Reader, 0)}
}

func (g *Generator) New() ulid.ULID {
	g.mu.Lock()
	defer g.mu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now().UTC()), g.entropy)
}

func New() ulid.ULID {
	return defaultGenerator.New()
}

var defaultGenerator = NewGenerator()
