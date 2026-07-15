package events

import "testing"

func TestNilBusIsSafe(t *testing.T) {
	var b *Bus
	b.Emit(EvPageRead, F{"page": 1}) // must not panic
	b.SetGate(nil)
	ch, cancel := b.Subscribe(4)
	if ch != nil {
		t.Fatal("nil bus should return nil channel")
	}
	cancel()
}

func TestOrderedDelivery(t *testing.T) {
	b := NewBus()
	ch, cancel := b.Subscribe(16)
	defer cancel()

	for i := 0; i < 5; i++ {
		b.Emit(EvPageWrite, F{"i": i})
	}
	var last uint64
	for i := 0; i < 5; i++ {
		ev := <-ch
		if ev.Seq <= last {
			t.Fatalf("seq not increasing: %d after %d", ev.Seq, last)
		}
		last = ev.Seq
		if ev.Type != EvPageWrite {
			t.Fatalf("wrong type %q", ev.Type)
		}
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	b := NewBus()
	ch, cancel := b.Subscribe(16)
	cancel()
	b.Emit(EvPageRead, nil)
	if _, ok := <-ch; ok {
		t.Fatal("expected closed channel after cancel")
	}
	cancel() // double-cancel must be safe
}

func TestSlowSubscriberNeverBlocksEngine(t *testing.T) {
	b := NewBus()
	ch, cancel := b.Subscribe(4)
	defer cancel()

	// Emit far more events than the buffer holds; nobody is reading.
	// This must not deadlock, and the newest event must survive.
	for i := 0; i < 1000; i++ {
		b.Emit(EvCacheHit, F{"i": i})
	}
	var got Event
	for {
		select {
		case ev := <-ch:
			got = ev
			continue
		default:
		}
		break
	}
	if got.Fields["i"].(int) != 999 {
		t.Fatalf("newest event lost, got i=%v", got.Fields["i"])
	}
}

func TestGateRunsBeforeEmit(t *testing.T) {
	b := NewBus()
	ran := 0
	b.SetGate(func() { ran++ })
	b.Emit(EvStmtStart, nil)
	b.Emit(EvStmtEnd, nil)
	if ran != 2 {
		t.Fatalf("gate ran %d times, want 2", ran)
	}
	b.SetGate(nil)
	b.Emit(EvStmtStart, nil)
	if ran != 2 {
		t.Fatal("gate ran after removal")
	}
}
