package marketstate

import (
	"testing"
	"time"
)

func TestLadderRefusesABookNoWholeHasSeeded(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	if _, _, synced := b.Ladder(10); synced {
		t.Fatal("served a ladder from an unseeded book")
	}
}

func TestLadderIsBestFirstOnBothSides(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	err := b.ApplyDepthWhole(whole(1, time.Now(),
		[][2]string{{"207.50", "30"}, {"207.55", "80"}, {"207.40", "10"}},
		[][2]string{{"207.60", "100"}, {"207.58", "50"}, {"207.62", "200"}}))
	if err != nil {
		t.Fatal(err)
	}

	bids, asks, synced := b.Ladder(10)
	if !synced {
		t.Fatal("book should be synced after a whole")
	}
	// Best bid is the highest, best ask the lowest. A simulator that walks
	// these in the wrong order fills at the far side of the book and reports
	// a profit that is purely an ordering bug.
	if bids[0].Price != 207.55 || bids[1].Price != 207.50 || bids[2].Price != 207.40 {
		t.Errorf("bids not descending: %v", bids)
	}
	if asks[0].Price != 207.58 || asks[1].Price != 207.60 || asks[2].Price != 207.62 {
		t.Errorf("asks not ascending: %v", asks)
	}
	if bids[0].Amount != 80 {
		t.Errorf("best bid amount = %v, want 80", bids[0].Amount)
	}

	if _, asks, _ := b.Ladder(2); len(asks) != 2 {
		t.Errorf("Ladder(2) returned %d ask levels", len(asks))
	}
}

func TestPrintsAreDeliveredOnceEvenWhenTheyShareAMillisecond(t *testing.T) {
	t.Parallel()
	// bitbank stamps prints in milliseconds and batches them, so a burst can
	// share one. Cursoring on time would drop the rest of that millisecond or
	// replay it; either one corrupts a queue model.
	b := NewBook("xrp_jpy")
	at := time.Now().UTC()
	raw := txs(
		Trade{ID: 101, At: at, Side: "sell", Price: 207.50, Amount: 1},
		Trade{ID: 102, At: at, Side: "sell", Price: 207.50, Amount: 2},
		Trade{ID: 103, At: at, Side: "buy", Price: 207.60, Amount: 3},
	)
	if err := b.ApplyTransactions(raw); err != nil {
		t.Fatal(err)
	}

	got, cursor := b.TradesAfter(0)
	if len(got) != 3 {
		t.Fatalf("got %d prints, want 3", len(got))
	}
	if cursor != 103 {
		t.Errorf("cursor = %d, want 103", cursor)
	}
	if again, _ := b.TradesAfter(cursor); len(again) != 0 {
		t.Errorf("replayed %d prints already consumed", len(again))
	}

	// A partially-consumed burst resumes mid-millisecond.
	rest, _ := b.TradesAfter(101)
	if len(rest) != 2 || rest[0].ID != 102 {
		t.Errorf("resume from 101 gave %v", rest)
	}
}
