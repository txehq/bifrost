package cloudflare

import "testing"

func BenchmarkIsClefModelName(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if !isClefModelName("clef-flash") {
			b.Fatal("a plain model name was refused")
		}
	}
}
