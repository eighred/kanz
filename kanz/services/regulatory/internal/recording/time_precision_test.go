package recording

import (
	"bytes"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestPublishRecordingTimeMatchesPinnedPostgresCodec(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sample, want time.Time
	}{
		{"fraction", time.Date(2026, 11, 17, 12, 0, 0, 123456789, time.UTC), time.Date(2026, 11, 17, 12, 0, 0, 123456000, time.UTC)},
		{"offset", time.Date(2026, 11, 17, 12, 0, 0, 999999999, time.FixedZone("fixture", 3*3600)), time.Date(2026, 11, 17, 9, 0, 0, 999999000, time.UTC)},
		{"before-epoch", time.Unix(0, -1), time.Unix(-1, 999999000).UTC()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := recordingTime(tc.sample)
			if !got.Equal(tc.want) || got.Location() != time.UTC || recordingTime(got) != got {
				t.Fatal("single recording sample was not normalized to the literal UTC microsecond instant")
			}
			codec := pgtype.NewMap()
			raw, err := codec.Encode(pgtype.TimestamptzOID, pgtype.BinaryFormatCode, tc.sample, nil)
			if err != nil || len(raw) != 8 {
				t.Fatal("pinned pgx timestamp sample encoding failed")
			}
			var decoded time.Time
			if err := codec.Scan(pgtype.TimestamptzOID, pgtype.BinaryFormatCode, raw, &decoded); err != nil || !decoded.Equal(tc.want) || decoded.Equal(tc.sample) {
				t.Fatal("raw nanosecond sample did not expose the pinned codec's precision loss")
			}
			normalized, err := codec.Encode(pgtype.TimestamptzOID, pgtype.BinaryFormatCode, got, nil)
			if err != nil || !bytes.Equal(normalized, raw) {
				t.Fatal("normalized recording sample changed its durable timestamp")
			}
			if err := codec.Scan(pgtype.TimestamptzOID, pgtype.BinaryFormatCode, normalized, &decoded); err != nil || !decoded.Equal(got) {
				t.Fatal("normalized recording sample did not survive the pinned codec exactly")
			}
		})
	}
}
