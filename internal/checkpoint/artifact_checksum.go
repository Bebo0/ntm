package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// ArtifactChecksum fingerprints the stored bytes of a checkpoint artifact.
// Compressed scrollback is hashed without decompression. This detects accidental
// corruption; it is not an authenticity signature for untrusted checkpoints.
type ArtifactChecksum struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

func (c ArtifactChecksum) validate() error {
	if c.SizeBytes < 0 {
		return fmt.Errorf("negative artifact size: %d", c.SizeBytes)
	}
	if len(c.SHA256) != sha256.Size*2 {
		return fmt.Errorf("invalid SHA-256 digest length: %d", len(c.SHA256))
	}
	if _, err := hex.DecodeString(c.SHA256); err != nil {
		return fmt.Errorf("invalid SHA-256 digest: %w", err)
	}
	return nil
}

func checksumArtifactFile(path string) (ArtifactChecksum, error) {
	return readArtifactChecksum(path, nil)
}

func verifyArtifactFile(path string, expected ArtifactChecksum) error {
	return verifyArtifactFileContext(context.Background(), path, expected)
}

func verifyArtifactFileContext(ctx context.Context, path string, expected ArtifactChecksum) error {
	if err := expected.validate(); err != nil {
		return err
	}
	actual, err := readArtifactChecksumContext(ctx, path, &expected)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual.SHA256, expected.SHA256) {
		return fmt.Errorf("SHA-256 mismatch: expected %s, got %s", expected.SHA256, actual.SHA256)
	}
	return nil
}

// readArtifactChecksum uses bounded memory and refuses non-regular files. The
// caller must first resolve the path within the checkpoint's directory. Check
// identity again after opening so a replaced final path is not silently hashed.
func readArtifactChecksum(path string, expected *ArtifactChecksum) (ArtifactChecksum, error) {
	return readArtifactChecksumContext(context.Background(), path, expected)
}

func readArtifactChecksumContext(ctx context.Context, path string, expected *ArtifactChecksum) (ArtifactChecksum, error) {
	var zero ArtifactChecksum
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return zero, err
	}
	if !before.Mode().IsRegular() {
		return zero, fmt.Errorf("artifact is not a regular file: %s", path)
	}
	if expected != nil && before.Size() != expected.SizeBytes {
		return zero, fmt.Errorf("size mismatch: expected %d bytes, got %d", expected.SizeBytes, before.Size())
	}

	file, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return zero, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return zero, fmt.Errorf("artifact changed while opening: %s", path)
	}

	hash := sha256.New()
	// Read at most the size observed before opening. A growing file cannot
	// keep verification running indefinitely; the final stat detects growth.
	n, err := io.Copy(hash, io.LimitReader(artifactContextReader{ctx: ctx, reader: file}, before.Size()))
	if err != nil {
		return zero, fmt.Errorf("reading artifact: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	after, err := file.Stat()
	if err != nil {
		return zero, err
	}
	if n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return zero, fmt.Errorf("artifact changed while hashing: %s", path)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return zero, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return zero, fmt.Errorf("artifact replaced while hashing: %s", path)
	}
	return ArtifactChecksum{SHA256: hex.EncodeToString(hash.Sum(nil)), SizeBytes: n}, nil
}

// Read checks cancellation between bounded chunks; verification never needs
// to buffer an entire pane transcript or finish hashing after cancellation.
type artifactContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r artifactContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func checksumArtifactBytes(data []byte) ArtifactChecksum {
	hash := sha256.Sum256(data)
	return ArtifactChecksum{SHA256: hex.EncodeToString(hash[:]), SizeBytes: int64(len(data))}
}

func verifyArtifactBytes(data []byte, expected ArtifactChecksum) error {
	if err := expected.validate(); err != nil {
		return err
	}
	if int64(len(data)) != expected.SizeBytes {
		return fmt.Errorf("size mismatch: expected %d bytes, got %d", expected.SizeBytes, len(data))
	}
	actual := checksumArtifactBytes(data)
	if !strings.EqualFold(actual.SHA256, expected.SHA256) {
		return fmt.Errorf("SHA-256 mismatch: expected %s, got %s", expected.SHA256, actual.SHA256)
	}
	return nil
}
