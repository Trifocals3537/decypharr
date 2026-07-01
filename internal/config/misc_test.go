package config

import "testing"

func TestParseSizeSupportsExpandedUnits(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int64
	}{
		{name: "bare number", in: "1024", want: 1024},
		{name: "bytes", in: "512B", want: 512},
		{name: "kilobytes", in: "1KB", want: 1024},
		{name: "megabytes with spaces", in: " 2 mb ", want: 2 * 1024 * 1024},
		{name: "decimal gigabytes", in: "1.5GB", want: 1536 * 1024 * 1024},
		{name: "terabytes", in: "3TB", want: 3 * 1024 * 1024 * 1024 * 1024},
		{name: "decimal petabytes", in: "1.25PB", want: 1280 * 1024 * 1024 * 1024 * 1024},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSize(tc.in)
			if err != nil {
				t.Fatalf("ParseSize(%q) returned error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseSizeRejectsInvalidInput(t *testing.T) {
	if _, err := ParseSize("not-a-size"); err == nil {
		t.Fatal("expected invalid size to return an error")
	}
}
