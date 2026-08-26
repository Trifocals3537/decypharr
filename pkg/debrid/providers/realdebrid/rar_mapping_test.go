package realdebrid

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/pkg/debrid/common/rar"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func storedRARFile(path string, offset, size int64) *rar.File {
	return &rar.File{
		Path:           path,
		Size:           size,
		CompressedSize: size,
		Method:         rar.MethodStore,
		DataOffset:     offset,
	}
}

func TestMapStoredRARFilesUsesValidatedArchiveMetadata(t *testing.T) {
	generated := time.Unix(100, 0)
	selected := []types.File{{
		TorrentId: "torrent", Id: "7", Name: "Video?.mkv", Path: "Release/Video?.mkv", Size: 999,
	}}

	files, err := mapStoredRARFiles(
		selected,
		[]*rar.File{storedRARFile("Archive/Video_.mkv", 10, 4)},
		"https://restricted.example/archive",
		generated,
	)
	if err != nil {
		t.Fatal(err)
	}
	file, exists := files["Video?.mkv"]
	if !exists || file.Size != 4 || !file.IsRar || file.ByteRange == nil ||
		*file.ByteRange != [2]int64{10, 13} || file.Link != "https://restricted.example/archive" ||
		!file.Generated.Equal(generated) {
		t.Fatalf("mapped file = %#v, exists=%v", file, exists)
	}
	if selected[0].Size != 999 || selected[0].ByteRange != nil || selected[0].IsRar {
		t.Fatalf("input selection was mutated: %#v", selected[0])
	}
}

func TestMapStoredRARFilesFailsClosedOnUnsafeMappings(t *testing.T) {
	tests := []struct {
		name          string
		selected      []types.File
		rarFiles      []*rar.File
		wantError     error
		wantSubstring string
	}{
		{
			name:     "compressed entry",
			selected: []types.File{{Name: "video.mkv", Size: 8}},
			rarFiles: []*rar.File{{
				Path: "video.mkv", Size: 8, CompressedSize: 4, Method: 0x33, DataOffset: 10,
			}},
			wantError: rar.ErrCompressionNotSupported,
		},
		{
			name:          "incomplete match",
			selected:      []types.File{{Name: "one.mkv"}, {Name: "two.mkv"}},
			rarFiles:      []*rar.File{storedRARFile("one.mkv", 10, 4)},
			wantSubstring: "matched 1 of 2",
		},
		{
			name:          "duplicate archive basename",
			selected:      []types.File{{Name: "video.mkv"}},
			rarFiles:      []*rar.File{storedRARFile("one/video.mkv", 10, 4), storedRARFile("two/video.mkv", 20, 4)},
			wantSubstring: "matches multiple archive entries",
		},
		{
			name:          "ambiguous selected safe basename",
			selected:      []types.File{{Name: "Video?.mkv"}, {Name: "Video*.mkv"}},
			rarFiles:      []*rar.File{storedRARFile("Video_.mkv", 10, 4)},
			wantSubstring: "ambiguous selected basename",
		},
		{
			name:          "no selected match",
			selected:      []types.File{{Name: "video.mkv"}},
			rarFiles:      []*rar.File{storedRARFile("other.mkv", 10, 4)},
			wantSubstring: "no directly streamable selected files",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files, err := mapStoredRARFiles(test.selected, test.rarFiles, "restricted", time.Time{})
			if err == nil || files != nil {
				t.Fatalf("mapStoredRARFiles() = %#v, %v; want failure", files, err)
			}
			if test.wantError != nil && !errors.Is(err, test.wantError) {
				t.Fatalf("error = %v, want %v", err, test.wantError)
			}
			if test.wantSubstring != "" && !strings.Contains(err.Error(), test.wantSubstring) {
				t.Fatalf("error = %v, want substring %q", err, test.wantSubstring)
			}
		})
	}
}
