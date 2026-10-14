package uuid

import "testing"

func TestNewIsAValidVersion4UUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := New()
		if err != nil {
			t.Fatal(err)
		}
		if !Valid(id) || id[14] != '4' || !(id[19] == '8' || id[19] == '9' || id[19] == 'a' || id[19] == 'b') {
			t.Fatalf("not a version 4 UUID: %q", id)
		}
		if seen[id] {
			t.Fatalf("repeated: %q", id)
		}
		seen[id] = true
	}
}

func TestValid(t *testing.T) {
	for s, want := range map[string]bool{
		"4f1c2a3b-5d6e-4f70-8a9b-0c1d2e3f4a5b": true,
		"4F1C2A3B-5D6E-4F70-8A9B-0C1D2E3F4A5B": true,
		"4f1c2a3b5d6e4f708a9b0c1d2e3f4a5b":     false,
		"4f1c2a3b-5d6e-4f70-8a9b-0c1d2e3f4a5":  false,
		"4f1c2a3b-5d6e-4f70-8a9b-0c1d2e3f4a5g": false,
		"laptop":                               false,
		"":                                     false,
	} {
		if got := Valid(s); got != want {
			t.Errorf("Valid(%q) = %v, want %v", s, got, want)
		}
	}
}
