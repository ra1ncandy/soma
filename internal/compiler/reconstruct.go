package compiler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ra1ncandy/soma/internal/store"
)

// ErrFrameHashMismatch indicates reconstructed wire payload SHA-256 differs from recorded CompiledFrameHash.
var ErrFrameHashMismatch = errors.New("compiler: reconstructed frame hash mismatch")

// SnapshotMetadata captures minimal metadata stored alongside EpochSnapshot for deterministic replay/X-Ray.
type SnapshotMetadata struct {
	ProviderFlavor string `json:"provider_flavor"`
}

// ReconstructFrame rebuilds the exact CompiledFrame for stepID from its EpochSnapshot and incremental
// FrameDelta chain, verifying SHA-256 bit-for-bit equality against step.CompiledFrameHash.
func ReconstructFrame(ctx context.Context, st *store.Store, stepID string, profile ModelProfile) (CompiledFrame, error) {
	target, err := st.GetStep(ctx, stepID)
	if err != nil {
		return CompiledFrame{}, fmt.Errorf("load target step %s: %w", stepID, err)
	}
	ancestry, err := st.GetAncestry(ctx, stepID)
	if err != nil {
		return CompiledFrame{}, fmt.Errorf("load ancestry %s: %w", stepID, err)
	}

	snapRow, err := st.GetEpochSnapshot(ctx, target.EpochSnapshotID)
	if err != nil {
		return CompiledFrame{}, fmt.Errorf("load epoch snapshot %s: %w", target.EpochSnapshotID, err)
	}
	l0Bytes, err := st.Blobs().Get(snapRow.Layer0BlobHash)
	if err != nil {
		return CompiledFrame{}, fmt.Errorf("load layer0 blob: %w", err)
	}
	l1Bytes, err := st.Blobs().Get(snapRow.Layer1BlobHash)
	if err != nil {
		return CompiledFrame{}, fmt.Errorf("load layer1 blob: %w", err)
	}

	var meta SnapshotMetadata
	if snapRow.MetadataJSON != "" {
		_ = json.Unmarshal([]byte(snapRow.MetadataJSON), &meta)
	}
	effectiveProfile := profile
	if meta.ProviderFlavor != "" {
		effectiveProfile.ProviderFlavor = meta.ProviderFlavor
	}

	snap := EpochSnapshot{
		Epoch:          snapRow.Epoch,
		Layer0Text:     string(l0Bytes),
		Layer0Hash:     snapRow.Layer0BlobHash,
		Layer1Text:     string(l1Bytes),
		Layer1Hash:     snapRow.Layer1BlobHash,
		PrefixHashL0L1: snapRow.PrefixHashL0L1,
	}

	var l2 []LogEntry
	var finalTail LiveTailState
	for _, s := range ancestry {
		if s.EpochSnapshotID != target.EpochSnapshotID {
			continue
		}
		deltaBytes, err := st.Blobs().Get(s.FrameDeltaBlobHash)
		if err != nil {
			return CompiledFrame{}, fmt.Errorf("load frame delta %s: %w", s.ID, err)
		}
		var delta FrameDelta
		if err := json.Unmarshal(deltaBytes, &delta); err != nil {
			return CompiledFrame{}, fmt.Errorf("decode frame delta %s: %w", s.ID, err)
		}
		l2 = append(l2, delta.AppendedL2...)
		finalTail = delta.LiveTail
	}

	if finalTail.Viewport != nil && finalTail.Viewport.BlobHash != "" {
		if imgBytes, bErr := st.Blobs().Get(finalTail.Viewport.BlobHash); bErr == nil {
			mime := finalTail.Viewport.MimeType
			if mime == "" {
				mime = "image/png"
			}
			finalTail.Viewport.DataURI = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(imgBytes)
		}
	}

	frame := Compile(snap, l2, finalTail, effectiveProfile)
	if frame.CompiledFrameHash != target.CompiledFrameHash {
		return CompiledFrame{}, fmt.Errorf("%w: got %s want %s", ErrFrameHashMismatch, frame.CompiledFrameHash, target.CompiledFrameHash)
	}
	return frame, nil
}
