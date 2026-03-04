package events

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPublishReceive(t *testing.T) {
	bus := NewBus()
	sub := bus.Subscribe(Filter{})
	defer bus.Unsubscribe(sub)

	bus.Publish(Event{Type: UsageUpdate, ContainerID: "c1", Timestamp: time.Now()})

	select {
	case e := <-sub.C:
		if e.Type != UsageUpdate || e.ContainerID != "c1" {
			t.Errorf("got type=%s container=%s, want usage_update c1", e.Type, e.ContainerID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestFilterByType(t *testing.T) {
	bus := NewBus()
	sub := bus.Subscribe(Filter{Types: []EventType{LimitChange}})
	defer bus.Unsubscribe(sub)

	bus.Publish(Event{Type: UsageUpdate, ContainerID: "c1"})
	bus.Publish(Event{Type: LimitChange, ContainerID: "c1"})

	select {
	case e := <-sub.C:
		if e.Type != LimitChange {
			t.Errorf("got type=%s, want limit_change", e.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	// Should not have received the usage_update
	select {
	case e := <-sub.C:
		t.Errorf("unexpected event: %s", e.Type)
	case <-time.After(50 * time.Millisecond):
		// ok
	}
}

func TestFilterByContainer(t *testing.T) {
	bus := NewBus()
	sub := bus.Subscribe(Filter{ContainerIDs: []string{"c2"}})
	defer bus.Unsubscribe(sub)

	bus.Publish(Event{Type: UsageUpdate, ContainerID: "c1"})
	bus.Publish(Event{Type: UsageUpdate, ContainerID: "c2"})

	select {
	case e := <-sub.C:
		if e.ContainerID != "c2" {
			t.Errorf("got container=%s, want c2", e.ContainerID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	select {
	case e := <-sub.C:
		t.Errorf("unexpected event for container %s", e.ContainerID)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCombinedFilters(t *testing.T) {
	bus := NewBus()
	sub := bus.Subscribe(Filter{
		ContainerIDs: []string{"c1"},
		Types:        []EventType{LimitChange},
	})
	defer bus.Unsubscribe(sub)

	bus.Publish(Event{Type: UsageUpdate, ContainerID: "c1"})    // wrong type
	bus.Publish(Event{Type: LimitChange, ContainerID: "c2"})    // wrong container
	bus.Publish(Event{Type: LimitChange, ContainerID: "c1"})    // match

	select {
	case e := <-sub.C:
		if e.Type != LimitChange || e.ContainerID != "c1" {
			t.Errorf("got type=%s container=%s", e.Type, e.ContainerID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	select {
	case e := <-sub.C:
		t.Errorf("unexpected event: type=%s container=%s", e.Type, e.ContainerID)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	bus := NewBus()
	sub := bus.Subscribe(Filter{})
	bus.Unsubscribe(sub)

	_, open := <-sub.C
	if open {
		t.Error("channel should be closed after unsubscribe")
	}
}

func TestNonBlockingOnFullBuffer(t *testing.T) {
	bus := NewBus()
	sub := bus.Subscribe(Filter{})
	defer bus.Unsubscribe(sub)

	// Fill the buffer (capacity 64)
	for i := 0; i < 64; i++ {
		bus.Publish(Event{Type: UsageUpdate, ContainerID: "c1"})
	}

	// This should not block — event is dropped
	done := make(chan struct{})
	go func() {
		bus.Publish(Event{Type: UsageUpdate, ContainerID: "c1"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on full buffer")
	}
}

func TestMultipleSubscribers(t *testing.T) {
	bus := NewBus()
	sub1 := bus.Subscribe(Filter{})
	sub2 := bus.Subscribe(Filter{})
	defer bus.Unsubscribe(sub1)
	defer bus.Unsubscribe(sub2)

	bus.Publish(Event{Type: LimitChange, ContainerID: "c1"})

	for _, sub := range []*Subscriber{sub1, sub2} {
		select {
		case e := <-sub.C:
			if e.Type != LimitChange {
				t.Errorf("got type=%s, want limit_change", e.Type)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out")
		}
	}
}

func TestPublishDataMarshaling(t *testing.T) {
	bus := NewBus()
	sub := bus.Subscribe(Filter{})
	defer bus.Unsubscribe(sub)

	bus.PublishData(LimitChange, "c1", LimitChangeData{
		LimitType: "cpu",
		OldValue:  100,
		NewValue:  200,
		Operation: "increase",
	})

	select {
	case e := <-sub.C:
		if e.Type != LimitChange {
			t.Fatalf("type = %s, want limit_change", e.Type)
		}
		var data LimitChangeData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if data.LimitType != "cpu" || data.OldValue != 100 || data.NewValue != 200 || data.Operation != "increase" {
			t.Errorf("unexpected data: %+v", data)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}
