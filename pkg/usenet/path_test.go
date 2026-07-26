package usenet

import (
	"testing"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestFlattenLogicalNZBFileName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "normal release",
			input: "[Group] Show Name - S01E02 [1080p].mkv",
			want:  "[Group] Show Name - S01E02 [1080p].mkv",
		},
		{
			name:  "archive slash path",
			input: "Season 01/Episode 01.mkv",
			want:  "Episode 01.mkv",
		},
		{
			name:  "archive backslash path",
			input: `Season 01\Episode 01.mkv`,
			want:  "Episode 01.mkv",
		},
		{
			name:  "archive traversal is flattened",
			input: "../../Episode 01.mkv",
			want:  "Episode 01.mkv",
		},
		{
			name:    "empty",
			input:   "",
			wantErr: true,
		},
		{
			name:    "control character leaf",
			input:   "Season 01/bad\nname.mkv",
			wantErr: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := flattenLogicalNZBFileName(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("flattenLogicalNZBFileName(%q) error = nil", test.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("flattenLogicalNZBFileName(%q) error = %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("flattenLogicalNZBFileName(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestNormalizeLogicalNZBFileNamesRejectsFlattenedCollision(t *testing.T) {
	files := []storage.NZBFile{
		{Name: "Season 01/Episode.mkv"},
		{Name: "Extras/Episode.mkv"},
	}
	if err := normalizeLogicalNZBFileNames(files); err == nil {
		t.Fatal("normalizeLogicalNZBFileNames() accepted colliding flattened names")
	}
}

func TestNormalizeLogicalNZBFileNamesRejectsPortableCaseCollision(t *testing.T) {
	files := []storage.NZBFile{
		{Name: "Episode.MKV"},
		{Name: "episode.mkv"},
	}
	if err := normalizeLogicalNZBFileNames(files); err == nil {
		t.Fatal("normalizeLogicalNZBFileNames() accepted a case-insensitive collision")
	}
}
