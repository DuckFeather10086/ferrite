package store

import (
	"context"
	"testing"
	"time"
)

func TestAudioOffset_RoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if _, ok, err := s.AudioOffsetFor(ctx, "mx"); err != nil || ok {
		t.Fatalf("empty table: ok=%v err=%v", ok, err)
	}

	if err := s.PutAudioOffset(ctx, "mx", 0.1467); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.AudioOffsetFor(ctx, "mx")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.OffsetS != 0.1467 || got.Channel != "mx" {
		t.Fatalf("got %+v", got)
	}
	if got.Age() > time.Minute || got.Age() < 0 {
		t.Fatalf("age = %v, want ~0", got.Age())
	}

	// Re-measuring replaces rather than duplicating.
	if err := s.PutAudioOffset(ctx, "mx", 0.3025); err != nil {
		t.Fatal(err)
	}
	got, _, err = s.AudioOffsetFor(ctx, "mx")
	if err != nil {
		t.Fatal(err)
	}
	if got.OffsetS != 0.3025 {
		t.Fatalf("offset = %v, want the newer measurement", got.OffsetS)
	}
	all, err := s.ListAudioOffsets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("rows = %+v, want 1", all)
	}

	// Forget forces a re-probe; forgetting twice is fine.
	if err := s.ForgetAudioOffset(ctx, "mx"); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetAudioOffset(ctx, "mx"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.AudioOffsetFor(ctx, "mx"); ok {
		t.Fatal("offset survived ForgetAudioOffset")
	}
}

func TestAudioOffset_NegativeAndPerChannel(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// A negative skew (audio behind video) must survive the round trip.
	if err := s.PutAudioOffset(ctx, "nhk", -0.42); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAudioOffset(ctx, "tbs", 0.5); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.AudioOffsetFor(ctx, "nhk")
	if err != nil {
		t.Fatal(err)
	}
	if got.OffsetS != -0.42 {
		t.Fatalf("offset = %v, want -0.42", got.OffsetS)
	}
	all, err := s.ListAudioOffsets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Channel != "nhk" || all[1].Channel != "tbs" {
		t.Fatalf("rows = %+v, want nhk then tbs", all)
	}
}
