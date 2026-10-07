package checkpoint

import (
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
	if err := expected.validate(); err != nil {
		return err
	}
	actual, err := readArtifactChecksum(path, &expected)
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
	var zero ArtifactChecksum
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
	n, err := io.Copy(hash, io.LimitReader(file, before.Size()))
	if err != nil {
		return zero, fmt.Errorf("reading artifact: %w", err)
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
