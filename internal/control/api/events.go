package api

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/garysng/bean/internal/control/store"
)

// eventBus fans lifecycle events out to live subscribers. Persistence is
// the store's job; this only serves the streaming path, so a slow client
// is dropped rather than allowed to block emitters.
type eventBus struct {
	mu     sync.Mutex
	nextID uint64
	subs   map[uint64]*subscription
}

type subscription struct {
	ch chan *store.Event
	// sandboxID and labelKey/labelVal filter what the subscriber receives;
	// empty means no filtering on that dimension.
	sandboxID string
	labelKey  string
	labelVal  string
	dropped   atomic.Uint64
}

const subscriberBuffer = 64

func newEventBus() *eventBus {
	return &eventBus{subs: map[uint64]*subscription{}}
}

// subscribe registers a subscriber and returns its channel plus an
// unsubscribe func. Filters are matched against each published event.
func (b *eventBus) subscribe(sandboxID, labelKey, labelVal string) (*subscription, func()) {
	sub := &subscription{
		ch:        make(chan *store.Event, subscriberBuffer),
		sandboxID: sandboxID,
		labelKey:  labelKey,
		labelVal:  labelVal,
	}
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = sub
	b.mu.Unlock()

	return sub, func() {
		b.mu.Lock()
		if s, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(s.ch)
		}
		b.mu.Unlock()
	}
}

// publish delivers an event to matching subscribers. Full channels are
// skipped (and counted) so one stuck client cannot stall the API.
func (b *eventBus) publish(ev *store.Event, labels map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sub := range b.subs {
		if sub.sandboxID != "" && sub.sandboxID != ev.SandboxID {
			continue
		}
		if sub.labelKey != "" && labels[sub.labelKey] != sub.labelVal {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			sub.dropped.Add(1)
		}
	}
}

// subscriberCount reports live subscribers (metrics/tests).
func (b *eventBus) subscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// parseLabelFilter splits a "key=value" query parameter.
func parseLabelFilter(s string) (key, val string) {
	if s == "" {
		return "", ""
	}
	k, v, found := strings.Cut(s, "=")
	if !found {
		return k, ""
	}
	return k, v
}
