package checkpoint

import "fmt"

// prepareExportArtifactIntegrity describes the bytes that will actually be
// exported, not the source's omitted or pre-redaction payloads. The caller
// verifies the source checkpoint before preparing any transformed content.
func prepareExportArtifactIntegrity(dir string, cp *Checkpoint, prepared map[string][]byte) error {
	manifest := &ArtifactIntegrity{Version: artifactIntegrityVersion, Files: make(map[string]ArtifactChecksum)}
	for _, rel := range checkpointPayloadFiles(cp) {
		if data, ok := prepared[rel]; ok {
			manifest.Files[rel] = checksumArtifactBytes(data)
			continue
		}
		if cp.ArtifactIntegrity != nil {
			expected, ok := cp.ArtifactIntegrity.Files[rel]
			if !ok {
				return fmt.Errorf("%w: missing checksum for export artifact %q", ErrArtifactIntegrity, rel)
			}
			// Keep the verified source fingerprint. The archive writers also
			// verify the bytes they read, catching changes after preflight.
			manifest.Files[rel] = expected
			continue
		}
		path, err := resolveExistingCheckpointArtifactPath(dir, rel)
		if err != nil {
			return err
		}
		checksum, err := checksumArtifactFile(path)
		if err != nil {
			return fmt.Errorf("checksumming export artifact %q: %w", rel, err)
		}
		manifest.Files[rel] = checksum
	}
	// Assign a new manifest so export never mutates the original checkpoint.
	cp.ArtifactIntegrity = manifest
	return nil
}

func verifyExportArtifactBytes(cp *Checkpoint, rel string, data []byte) error {
	if cp.ArtifactIntegrity == nil {
		return nil
	}
	expected, ok := cp.ArtifactIntegrity.Files[rel]
	if !ok {
		return fmt.Errorf("%w: missing checksum for export artifact %q", ErrArtifactIntegrity, rel)
	}
	if err := verifyArtifactBytes(data, expected); err != nil {
		return fmt.Errorf("%w: export artifact %q: %v", ErrArtifactIntegrity, rel, err)
	}
	return nil
}

func verifyImportedArtifactIntegrity(cp *Checkpoint, files map[string][]byte) error {
	result := newIntegrityResult()
	cp.checkArtifactChecksumsUsing(result, func(rel string, expected ArtifactChecksum) error {
		data, ok := files[rel]
		if !ok {
			return fmt.Errorf("archive missing artifact %q", rel)
		}
		return verifyArtifactBytes(data, expected)
	})
	return artifactIntegrityError(result)
}
