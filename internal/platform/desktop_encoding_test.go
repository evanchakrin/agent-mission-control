package platform

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

func TestDesktopTaskFileEncoding(t *testing.T) {
	input := `<?xml version="1.0" encoding="UTF-8"?><Task>Owner é 🛰</Task>`
	b := desktopTaskFileBytes([]byte(input))
	if len(b)%2 != 0 || b[0] != 0xff || b[1] != 0xfe {
		t.Fatal("task file must have UTF-16LE BOM and complete code units")
	}
	u := make([]uint16, (len(b)-2)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2+i*2:])
	}
	want := `<?xml version="1.0" encoding="UTF-16"?><Task>Owner é 🛰</Task>`
	if got := string(utf16.Decode(u)); got != want {
		t.Fatalf("task encoding round trip: got %q", got)
	}
}
