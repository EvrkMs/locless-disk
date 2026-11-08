package scenario

import (
	"os"
	"testing"
)

// TestFreeSpace asks Windows how much room is left by the temporary folder. It only
// asks; nothing on the disk changes.
func TestFreeSpace(t *testing.T) {
	free, err := freeSpace(os.TempDir())
	if err != nil || free == 0 {
		t.Fatalf("free %d, err %v", free, err)
	}
	t.Logf("free by %s: %d MiB", os.TempDir(), free>>20)
}
