package main

import (
	"errors"
	"strconv"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The dial needs a live broker, so these tests exercise the one thing a
// broker could not verify anyway: the folding logic in Update. Task 4/5 (or a
// broker-backed integration test elsewhere) cover the actual subscribe path.

func TestBusEventMsg_AppendsToEvents(t *testing.T) {
	m := newModel(Config{})
	row := lifecycleEvent{At: time.Now(), Type: "order.order.filled", OrderID: "order-1", Detail: "1 @ 50000"}

	next, _ := m.Update(busEventMsg(row))
	got := next.(model)

	if len(got.events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(got.events))
	}
	if got.events[0].OrderID != "order-1" {
		t.Errorf("events[0].OrderID = %q, want %q", got.events[0].OrderID, "order-1")
	}
}

func TestBusEventMsg_CapsAt200DroppingOldest(t *testing.T) {
	var cur tea.Model = newModel(Config{})

	for i := 0; i < 201; i++ {
		row := lifecycleEvent{OrderID: orderIDFor(i)}
		next, _ := cur.Update(busEventMsg(row))
		cur = next
	}

	got := cur.(model)
	if len(got.events) != 200 {
		t.Fatalf("len(events) = %d, want 200 (capped)", len(got.events))
	}
	// 201 events were sent (indices 0..200); the cap must drop the OLDEST, so
	// index 0 ("order-0") is gone and the surviving window is 1..200.
	if got.events[0].OrderID != orderIDFor(1) {
		t.Errorf("events[0].OrderID = %q, want %q (oldest dropped)", got.events[0].OrderID, orderIDFor(1))
	}
	if got.events[len(got.events)-1].OrderID != orderIDFor(200) {
		t.Errorf("events[last].OrderID = %q, want %q", got.events[len(got.events)-1].OrderID, orderIDFor(200))
	}
}

func TestBusPositionMsg_UpsertLastWriteWins(t *testing.T) {
	m := newModel(Config{})

	first := position{Portfolio: "fund-alpha", Instrument: "BTC-USD", Quantity: "1", AvgPrice: "50000"}
	second := position{Portfolio: "fund-alpha", Instrument: "BTC-USD", Quantity: "1.5", AvgPrice: "51000"}

	next, _ := m.Update(busPositionMsg(first))
	next, _ = next.Update(busPositionMsg(second))
	got := next.(model)

	key := "fund-alpha/BTC-USD"
	row, ok := got.book[key]
	if !ok {
		t.Fatalf("book[%q] missing", key)
	}
	if row.Quantity != "1.5" || row.AvgPrice != "51000" {
		t.Errorf("book[%q] = %+v, want the SECOND write (last-write-wins)", key, row)
	}
	if len(got.book) != 1 {
		t.Errorf("len(book) = %d, want 1 (upsert, not append)", len(got.book))
	}
}

func TestBusErrMsg_SetsErrWithoutPanic(t *testing.T) {
	m := newModel(Config{})
	wantErr := errors.New("nats: dial refused")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Update panicked on busErrMsg: %v", r)
		}
	}()

	next, _ := m.Update(busErrMsg{err: wantErr})
	got := next.(model)

	if got.err == nil || got.err.Error() != wantErr.Error() {
		t.Errorf("err = %v, want %v", got.err, wantErr)
	}
	// The UI must remain usable: View must still render without panicking.
	_ = got.View()
}

func orderIDFor(i int) string {
	// Deterministic, order-preserving id so the cap test can assert exactly
	// which entries survived.
	return "order-" + strconv.Itoa(i)
}
