package events_test

import (
	"testing"

	"github.com/haileyok/botplaysbot/internal/events"
)

func moveEvent(uri string, ply int64) events.Event {
	ev := events.Event{Kind: events.KindMove, GameURI: uri}
	ev.Move.Ply = ply
	return ev
}

func finishedEvent(uri string) events.Event {
	ev := events.Event{Kind: events.KindGameFinished, GameURI: uri}
	ev.Result = &events.Result{Outcome: "draw", Reason: "agreement"}
	return ev
}

func TestSubscribePerGameFilter(t *testing.T) {
	bus := events.NewBus()
	got := bus.Subscribe(events.SubscribeOptions{GameURI: "at://did:plc:svc/bot.plays.bot.game/3kkk"})

	bus.Publish(moveEvent("at://did:plc:svc/bot.plays.bot.game/other", 1))
	bus.Publish(moveEvent("at://did:plc:svc/bot.plays.bot.game/3kkk", 1))
	bus.Publish(finishedEvent("at://did:plc:svc/bot.plays.bot.game/3kkk"))

	for _, want := range []string{"at://did:plc:svc/bot.plays.bot.game/3kkk", "at://did:plc:svc/bot.plays.bot.game/3kkk"} {
		ev, ok := <-got.C
		if !ok {
			t.Fatal("subscription closed early")
		}
		if ev.GameURI != want {
			t.Fatalf("got %s, want %s", ev.GameURI, want)
		}
	}
	select {
	case ev := <-got.C:
		t.Fatalf("unexpected extra event %+v", ev)
	default:
	}
	bus.Close(got)
	if _, ok := <-got.C; ok {
		t.Fatal("channel not closed by Close")
	}
}

func TestSubscribeKindFilter(t *testing.T) {
	bus := events.NewBus()
	got := bus.Subscribe(events.SubscribeOptions{Kinds: []string{events.KindGameFinished, events.KindGameStarted}})

	bus.Publish(moveEvent("at://x/game/1", 1))
	bus.Publish(events.Event{Kind: events.KindGameStarted, GameURI: "at://x/game/1"})
	bus.Publish(finishedEvent("at://x/game/1"))

	ev := <-got.C
	if ev.Kind != events.KindGameStarted {
		t.Fatalf("first = %s, want gameStarted", ev.Kind)
	}
	ev = <-got.C
	if ev.Kind != events.KindGameFinished || ev.Result == nil || ev.Result.Reason != "agreement" {
		t.Fatalf("second = %+v, want finished with result", ev)
	}
	bus.Close(got)
}

func TestNoFilterReceivesEverything(t *testing.T) {
	bus := events.NewBus()
	got := bus.Subscribe(events.SubscribeOptions{})
	bus.Publish(events.Event{Kind: events.KindDrawOffered, GameURI: "at://x/game/1", OfferedBy: "did:plc:a"})
	bus.Publish(moveEvent("at://x/game/2", 7))

	ev := <-got.C
	if ev.Kind != events.KindDrawOffered || ev.OfferedBy != "did:plc:a" {
		t.Fatalf("first = %+v", ev)
	}
	ev = <-got.C
	if ev.Move.Ply != 7 {
		t.Fatalf("second = %+v", ev)
	}
	bus.Close(got)
}

func TestSlowSubscriberDrops(t *testing.T) {
	bus := events.NewBus()
	got := bus.Subscribe(events.SubscribeOptions{})
	for i := 0; i < events.SubscribeBufferSize+5; i++ {
		bus.Publish(moveEvent("at://x/game/1", int64(i)))
	}
	// The channel holds SubscribeBuffer; the rest were dropped without
	// blocking, and the drop counter records them.
	if got.Dropped() == 0 {
		t.Fatal("no drops recorded for a stalled subscriber")
	}
	drained := 0
	for {
		select {
		case <-got.C:
			drained++
			continue
		default:
		}
		break
	}
	if drained != events.SubscribeBufferSize {
		t.Fatalf("drained %d, want %d", drained, events.SubscribeBufferSize)
	}
	bus.Close(got)
}

func TestClosedSubscriptionRemoved(t *testing.T) {
	bus := events.NewBus()
	got := bus.Subscribe(events.SubscribeOptions{})
	bus.Close(got)
	bus.Close(got) // double close is a no-op
	bus.Publish(moveEvent("at://x/game/1", 1))
	// No panic: the closed channel must not be sent to.
}
