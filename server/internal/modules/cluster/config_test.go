package cluster

import "testing"

func TestSameStringData(t *testing.T) {
	tests := []struct {
		name     string
		current  map[string]string
		next     map[string]string
		expected bool
	}{
		{name: "nil and empty are equivalent", current: nil, next: map[string]string{}, expected: true},
		{name: "same values", current: map[string]string{"a": "one"}, next: map[string]string{"a": "one"}, expected: true},
		{name: "changed value", current: map[string]string{"a": "one"}, next: map[string]string{"a": "two"}},
		{name: "changed empty key", current: map[string]string{"a": ""}, next: map[string]string{"b": ""}},
		{name: "removed key", current: map[string]string{"a": "one"}, next: map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if actual := sameStringMap(tt.current, tt.next); actual != tt.expected {
				t.Fatalf("sameStringMap() = %v, want %v", actual, tt.expected)
			}
		})
	}
}

func TestSameBytesData(t *testing.T) {
	tests := []struct {
		name     string
		current  map[string][]byte
		next     map[string][]byte
		expected bool
	}{
		{name: "nil and empty are equivalent", current: nil, next: map[string][]byte{}, expected: true},
		{name: "same values", current: map[string][]byte{"a": {0, 1}}, next: map[string][]byte{"a": {0, 1}}, expected: true},
		{name: "changed value", current: map[string][]byte{"a": {0, 1}}, next: map[string][]byte{"a": {0, 2}}},
		{name: "changed empty key", current: map[string][]byte{"a": {}}, next: map[string][]byte{"b": {}}},
		{name: "removed key", current: map[string][]byte{"a": {0, 1}}, next: map[string][]byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if actual := sameBytesData(tt.current, tt.next); actual != tt.expected {
				t.Fatalf("sameBytesData() = %v, want %v", actual, tt.expected)
			}
		})
	}
}
