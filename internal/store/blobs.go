package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

var (
	// ErrBlobCorrupt is returned when a retrieved blob fails SHA-256 verification.
	ErrBlobCorrupt = errors.New("store: blob sha256 mismatch")
	// ErrInvalidHash is returned when a blob hash is malformed.
	ErrInvalidHash = errors.New("store: invalid sha256 hash")
)

// BlobStore manages content-addressed zstd-compressed blobs on disk.
type BlobStore struct {
	root string
	enc  *zstd.Encoder
	dec  *zstd.Decoder
}

// NewBlobStore initializes a BlobStore under <root>/blobs.
func NewBlobStore(root string) (*BlobStore, error) {
	blobsDir := filepath.Join(root, "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create blobs dir: %w", err)
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, fmt.Errorf("init zstd encoder: %w", err)
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("init zstd decoder: %w", err)
	}
	return &BlobStore{
		root: blobsDir,
		enc:  enc,
		dec:  dec,
	}, nil
}

// Close releases encoder and decoder resources.
func (b *BlobStore) Close() error {
	var errEnc error
	if b.enc != nil {
		errEnc = b.enc.Close()
	}
	if b.dec != nil {
		b.dec.Close()
	}
	return errEnc
}

// Put stores data compressed with zstd and returns its SHA-256 hex digest and relative blob path.
func (b *BlobStore) Put(data []byte) (string, string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	rel := filepath.Join("blobs", hash[:2], hash+".zst")
	abs := filepath.Join(filepath.Dir(b.root), rel)

	if _, err := os.Stat(abs); err == nil {
		return hash, rel, nil
	}

	shardDir := filepath.Dir(abs)
	if err := os.MkdirAll(shardDir, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir blob shard: %w", err)
	}

	compressed := b.enc.EncodeAll(data, make([]byte, 0, len(data)/2+64))
	tmp, err := os.CreateTemp(shardDir, ".tmp-*.zst")
	if err != nil {
		return "", "", fmt.Errorf("create temp blob: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(compressed); err != nil {
		return "", "", fmt.Errorf("write temp blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", "", fmt.Errorf("close temp blob: %w", err)
	}
	if err := os.Rename(tmpName, abs); err != nil {
		// Content-addressed write race: if target already exists, ignore rename error.
		if _, statErr := os.Stat(abs); statErr != nil {
			return "", "", fmt.Errorf("rename blob: %w", err)
		}
		_ = os.Remove(tmpName)
	}
	cleanup = false
	return hash, rel, nil
}

// Get reads, decompresses, and verifies the SHA-256 checksum of a stored blob.
func (b *BlobStore) Get(hash string) ([]byte, error) {
	abs, err := b.absPath(hash)
	if err != nil {
		return nil, err
	}
	compressed, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", hash, err)
	}
	raw, err := b.dec.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("decode blob %s: %w", hash, err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, fmt.Errorf("%w: %s", ErrBlobCorrupt, hash)
	}
	return raw, nil
}

// Exists reports whether a blob with the given hash exists on disk.
func (b *BlobStore) Exists(hash string) bool {
	abs, err := b.absPath(hash)
	if err != nil {
		return false
	}
	_, err = os.Stat(abs)
	return err == nil
}

// TotalBytes returns the total compressed size on disk of all blobs in the store.
func (b *BlobStore) TotalBytes() (int64, error) {
	var total int64
	err := filepath.WalkDir(b.root, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func (b *BlobStore) absPath(hash string) (string, error) {
	if len(hash) != 64 {
		return "", fmt.Errorf("%w: length %d", ErrInvalidHash, len(hash))
	}
	for i := 0; i < len(hash); i++ {
		c := hash[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", fmt.Errorf("%w: invalid hex character", ErrInvalidHash)
		}
	}
	return filepath.Join(b.root, hash[:2], hash+".zst"), nil
}
