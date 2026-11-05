package proto

import "testing"

func TestBitmapSetHasCount(t *testing.T) {
	b, err := NewBitmap(1000)
	if err != nil {
		t.Fatalf("NewBitmap: %v", err)
	}
	if got := b.Blocks(); got != 1000 {
		t.Fatalf("Blocks: got %d, want 1000", got)
	}
	for _, i := range []uint64{0, 1, 7, 8, 999} {
		b.Set(i)
	}
	for _, i := range []uint64{0, 1, 7, 8, 999} {
		if !b.Has(i) {
			t.Fatalf("Has(%d) = false after Set", i)
		}
	}
	if b.Has(1000) {
		t.Fatal("Has(1000) = true on a 1000-block bitmap")
	}
	if got := b.Count(); got != 5 {
		t.Fatalf("Count: got %d, want 5", got)
	}
}

// TestBitmapKeepsExactSize uses a block count that is not a multiple of 8: the
// padding bits of the last byte must not count as clusters.
func TestBitmapKeepsExactSize(t *testing.T) {
	b, err := NewBitmap(1001)
	if err != nil {
		t.Fatalf("NewBitmap: %v", err)
	}
	if got := b.Blocks(); got != 1001 {
		t.Fatalf("Blocks: got %d, want 1001", got)
	}
	if got := len(b.Bytes()); got != 126 {
		t.Fatalf("Bytes: got %d bytes, want 126", got)
	}
	b.Set(1000)
	defer func() {
		if recover() == nil {
			t.Fatal("Set(1001) did not panic on a 1001-block bitmap")
		}
	}()
	b.Set(1001)
}

func TestNewBitmapRejectsZero(t *testing.T) {
	if _, err := NewBitmap(0); err == nil {
		t.Fatal("NewBitmap(0) returned no error")
	}
}

func TestParseBitmap(t *testing.T) {
	b, err := ParseBitmap([]byte{0xff, 0x01}, 9)
	if err != nil {
		t.Fatalf("ParseBitmap: %v", err)
	}
	if got := b.Count(); got != 9 {
		t.Fatalf("Count: got %d, want 9", got)
	}

	tests := []struct {
		name   string
		data   []byte
		blocks uint64
	}{
		{"no blocks", nil, 0},
		{"too short", []byte{0xff}, 9},
		{"too long", []byte{0xff, 0x01, 0x00}, 9},
		{"padding set", []byte{0xff, 0x03}, 9},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseBitmap(tc.data, tc.blocks); err == nil {
				t.Fatal("ParseBitmap accepted a bad bitmap")
			}
		})
	}
}
