package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactChecksumRoundTrip(t *testing.T) {
	for _, content := range []string{"", "abc", "\x00\xff\x01binary\x00", strings.Repeat("scrollback\n", 100000)} {
		t.Run(stringLengthName(len(content)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "artifact")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := checksumArtifactFile(path)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(content))
			if got.SHA256 != hex.EncodeToString(hash[:]) || got.SizeBytes != int64(len(content)) {
				t.Fatalf("unexpected fingerprint: %+v", got)
			}
			if err := verifyArtifactFile(path, got); err != nil {
				t.Fatalf("unchanged artifact rejected: %v", err)
			}
			got.SHA256 = strings.ToUpper(got.SHA256)
			if err := verifyArtifactFile(path, got); err != nil {
				t.Fatalf("valid uppercase digest rejected: %v", err)
			}
		})
	}
}

func stringLengthName(n int) string {
	// Keep test case names readable without embedding binary or large payloads.
	if n == 0 {
		return "empty"
	}
	if n < 10 {
		return "text"
	}
	if n < 100 {
		return "binary"
	}
	return "large"
}

func TestArtifactChecksumRejectsCorruption(t *testing.T) {
	for _, tc := range []struct {
		name, changed, want string
	}{
		{"same_size", "xyz", "SHA-256 mismatch"},
		{"truncated", "ab", "size mismatch"},
		{"appended", "abcd", "size mismatch"},
		{"emptied", "", "size mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "artifact")
			if err := os.WriteFile(path, []byte("abc"), 0600); err != nil {
				t.Fatal(err)
			}
			original, err := checksumArtifactFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.changed), 0600); err != nil {
				t.Fatal(err)
			}
			if err := verifyArtifactFile(path, original); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("verification error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestArtifactChecksumRejectsInvalidMetadata(t *testing.T) {
	for _, checksum := range []ArtifactChecksum{
		{SHA256: "", SizeBytes: 0},
		{SHA256: strings.Repeat("a", 63), SizeBytes: 0},
		{SHA256: strings.Repeat("z", 64), SizeBytes: 0},
		{SHA256: strings.Repeat("a", 64), SizeBytes: -1},
	} {
		if err := verifyArtifactFile("not-opened", checksum); err == nil {
			t.Fatalf("accepted invalid metadata: %+v", checksum)
		}
	}
}

func TestArtifactChecksumRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "regular")
	if err := os.WriteFile(file, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, filepath.Join(dir, "missing")} {
		if _, err := checksumArtifactFile(path); err == nil {
			t.Fatalf("accepted non-regular/missing path %q", path)
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := checksumArtifactFile(link); err == nil {
		t.Fatal("accepted a symlink")
	}
}
