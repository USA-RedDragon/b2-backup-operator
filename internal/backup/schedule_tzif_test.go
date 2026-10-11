package backup

import (
	"encoding/binary"
	"maps"
	"slices"
	"testing"
	"time"
)

func tzif(t *testing.T, offsets []int32, transitions []int32, types []uint8) *time.Location {
	t.Helper()
	b := []byte("TZif")
	b = append(b, make([]byte, 16)...)
	for _, n := range []int{0, 0, 0, len(transitions), len(offsets), 4 * len(offsets)} {
		b = binary.BigEndian.AppendUint32(b, uint32(n))
	}
	for _, at := range transitions {
		b = binary.BigEndian.AppendUint32(b, uint32(at))
	}
	b = append(b, types...)
	for i, off := range offsets {
		b = binary.BigEndian.AppendUint32(b, uint32(off))
		b = append(b, uint8(i), uint8(4*i))
	}
	for range offsets {
		b = append(b, "ZZZ\x00"...)
	}
	loc, err := time.LoadLocationFromTZData("Test/Zone", b)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestABoundThatKeepsTheOffsetMarksNoMinute(t *testing.T) {
	t.Parallel()
	forward := int32(time.Date(2026, 10, 3, 15, 30, 0, 0, time.UTC).Unix())
	loc := tzif(t, []int32{37800, 39600}, []int32{forward, 1<<31 - 1}, []uint8{1, 1})
	got := slices.Sorted(maps.Keys(locationTransitionMinutes(loc)))
	var want []int
	for m := 2 * 60; m < 2*60+30; m++ {
		want = append(want, m)
	}
	if !slices.Equal(got, want) {
		t.Errorf("got minutes %v, want %v", got, want)
	}
}
