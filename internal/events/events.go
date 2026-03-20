package events

import (
	"encoding/json"
	"sync"
	"time"
)

// EventType identifies the kind of event.
type EventType string

const (
	UsageUpdate        EventType = "usage_update"
	LimitChange        EventType = "limit_change"
	EnforcementChange  EventType = "enforcement_change"
	ContainerRegister  EventType = "container_register"
	ContainerRemove    EventType = "container_remove"
	OllamaEnqueue      EventType = "ollama_enqueue"
	OllamaDequeue      EventType = "ollama_dequeue"
	OllamaCancel       EventType = "ollama_cancel"
	OllamaBidChange    EventType = "ollama_bid_change"
	SystemSleep              EventType = "system_sleep"
	GlobalEnforcementChange  EventType = "global_enforcement_change"
	ContainerFrozen          EventType = "container_frozen"
	ContainerUnfrozen        EventType = "container_unfrozen"
	ProxyUpstreamError       EventType = "proxy_upstream_error"
)

// Event is the top-level envelope sent to subscribers.
type Event struct {
	Type        EventType       `json:"type"`
	ContainerID string          `json:"container_id"`
	Timestamp   time.Time       `json:"timestamp"`
	Data        json.RawMessage `json:"data"`
}

// UsageUpdateData is the payload for usage_update events.
type UsageUpdateData struct {
	Usage    map[string]int64 `json:"usage"`
	Limits   map[string]int64 `json:"limits"`
	Enforced map[string]bool  `json:"enforced"`
}

// LimitChangeData is the payload for limit_change events.
type LimitChangeData struct {
	LimitType string `json:"limit_type"`
	OldValue  int64  `json:"old_value"`
	NewValue  int64  `json:"new_value"`
	Operation string `json:"operation"`
}

// EnforcementChangeData is the payload for enforcement_change events.
type EnforcementChangeData struct {
	LimitType string `json:"limit_type"`
	Enforced  bool   `json:"enforced"`
}

// ContainerRegisterData is the payload for container_register events.
type ContainerRegisterData struct {
	DockerID string `json:"docker_id"`
	Name     string `json:"name"`
}

// ContainerRemoveData is the payload for container_remove events.
type ContainerRemoveData struct{}

// OllamaEnqueueData is the payload for ollama_enqueue events.
type OllamaEnqueueData struct {
	Model string `json:"model"`
	Bid   int64  `json:"bid"`
}

// OllamaDequeueData is the payload for ollama_dequeue events.
type OllamaDequeueData struct {
	Model       string  `json:"model"`
	WallSeconds float64 `json:"wall_seconds"`
	Cost        int64   `json:"cost"`
}

// OllamaCancelData is the payload for ollama_cancel events.
type OllamaCancelData struct {
	Reason string `json:"reason"` // "timeout", "cancelled", "removed"
}

// OllamaBidChangeData is the payload for ollama_bid_change events.
type OllamaBidChangeData struct {
	Bid int64 `json:"bid"`
}

// SystemSleepData is the payload for system_sleep events.
type SystemSleepData struct {
	DurationSeconds float64 `json:"duration_seconds"`
}

// GlobalEnforcementChangeData is the payload for global_enforcement_change events.
type GlobalEnforcementChangeData struct {
	LimitType string `json:"limit_type"`
	Enforced  bool   `json:"enforced"`
}

// ProxyUpstreamErrorData is the payload for proxy_upstream_error events.
type ProxyUpstreamErrorData struct {
	Host         string `json:"host"`
	StatusCode   int    `json:"status_code"`
	ErrorType    string `json:"error_type,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
}

// ContainerFrozenData is the payload for container_frozen events.
type ContainerFrozenData struct{}

// ContainerUnfrozenData is the payload for container_unfrozen events.
type ContainerUnfrozenData struct {
	EnforcementActive bool `json:"enforcement_active"`
}

// Filter controls which events a subscriber receives.
type Filter struct {
	ContainerIDs []string    // empty = all containers
	Types        []EventType // empty = all types
}

func (f *Filter) matches(e *Event) bool {
	if len(f.ContainerIDs) > 0 {
		found := false
		for _, id := range f.ContainerIDs {
			if id == e.ContainerID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(f.Types) > 0 {
		found := false
		for _, t := range f.Types {
			if t == e.Type {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Subscriber receives events on its channel.
type Subscriber struct {
	C      <-chan Event
	ch     chan Event
	filter Filter
}

// Bus is a thread-safe event pub/sub hub.
type Bus struct {
	mu          sync.RWMutex
	subscribers map[*Subscriber]struct{}
}

// NewBus creates a new event bus.
func NewBus() *Bus {
	return &Bus{
		subscribers: make(map[*Subscriber]struct{}),
	}
}

// Subscribe creates a new subscriber with the given filter.
func (b *Bus) Subscribe(filter Filter) *Subscriber {
	ch := make(chan Event, 64)
	sub := &Subscriber{
		C:      ch,
		ch:     ch,
		filter: filter,
	}
	b.mu.Lock()
	b.subscribers[sub] = struct{}{}
	b.mu.Unlock()
	return sub
}

// Unsubscribe removes a subscriber and closes its channel.
func (b *Bus) Unsubscribe(sub *Subscriber) {
	b.mu.Lock()
	if _, ok := b.subscribers[sub]; ok {
		delete(b.subscribers, sub)
		close(sub.ch)
	}
	b.mu.Unlock()
}

// Publish sends an event to all matching subscribers. Non-blocking: if a
// subscriber's buffer is full the event is dropped for that subscriber.
func (b *Bus) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for sub := range b.subscribers {
		if sub.filter.matches(&e) {
			select {
			case sub.ch <- e:
			default:
				// drop — subscriber is not keeping up
			}
		}
	}
}

// PublishData is a convenience wrapper that marshals the payload and publishes.
func (b *Bus) PublishData(typ EventType, containerID string, data interface{}) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	b.Publish(Event{
		Type:        typ,
		ContainerID: containerID,
		Timestamp:   time.Now(),
		Data:        raw,
	})
}
