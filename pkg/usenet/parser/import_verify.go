package parser

import (
	"context"
	"fmt"
	"math"

	"github.com/sirrobot01/decypharr/internal/crypto"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// verifyImportFile validates the logical map and actual sampled article bodies.
// STAT can succeed when BODY and ARTICLE both fail. Sampling is deliberately
// not a complete-availability guarantee; 100 percent opts into reading all
// articles. Each file is checked sequentially so probes do not fan out beyond
// the parser's existing processing connection budget.
func (p *NZBParser) verifyImportFile(ctx context.Context, file *storage.NZBFile, percent int) error {
	mappedSize, err := storedStreamSize(file.Size, file.IsEncrypted)
	if err != nil {
		return err
	}
	var offset int64
	for _, seg := range file.Segments {
		if seg.MessageID == "" || seg.Bytes <= 0 || seg.StartOffset != offset || offset > mappedSize || seg.Bytes > mappedSize-offset || seg.EndOffset != offset+seg.Bytes-1 || seg.SegmentDataStart < 0 {
			return fmt.Errorf("invalid logical segment map for %q", file.Name)
		}
		offset += seg.Bytes
	}
	if file.Size <= 0 || offset != mappedSize {
		return fmt.Errorf("incomplete logical segment map for %q", file.Name)
	}
	count := len(file.Segments)
	samples := min(count, max(3, count*min(100, max(1, percent))/100))
	for i := 0; i < samples; i++ {
		index := 0
		if samples > 1 {
			index = (count - 1) * i / (samples - 1)
		}
		seg := file.Segments[index]
		if err := ctx.Err(); err != nil {
			return err
		}
		var meta *nntp.YencMetadata
		err := p.manager.ExecuteWithFailover(ctx, func(conn *nntp.Connection) error {
			var err error
			meta, err = conn.GetHeaderPrefix(seg.MessageID, 0)
			return err
		})
		if err != nil {
			return fmt.Errorf("import body sample %d of %q: %w", index+1, file.Name, err)
		}
		if meta == nil || meta.Begin < 1 || meta.End < meta.Begin || meta.End > meta.Size || meta.PartSize != meta.DecodedSize || meta.End-meta.Begin+1 != meta.DecodedSize ||
			seg.SegmentDataStart > meta.DecodedSize || seg.Bytes > meta.DecodedSize-seg.SegmentDataStart ||
			(meta.Part > 0 && meta.Part != int64(seg.Number) && meta.Part != int64(seg.Number)+1) {
			return fmt.Errorf("invalid yEnc geometry in import body sample %d of %q", index+1, file.Name)
		}
	}
	return nil
}

// AES-CBC readers need the final complete cipher block even though the
// advertised logical size excludes padding. No other short/extra data is valid.
func storedStreamSize(size int64, encrypted bool) (int64, error) {
	if size < 0 {
		return 0, fmt.Errorf("negative stored file size")
	}
	if encrypted && size%crypto.BlockSize != 0 {
		padding := int64(crypto.BlockSize) - size%crypto.BlockSize
		if size > math.MaxInt64-padding {
			return 0, fmt.Errorf("stored file size overflow")
		}
		size += padding
	}
	return size, nil
}
