package recon

import (
	"context"
	"path/filepath"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/applier"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/differ"
	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/sopscrypt"
)

// repoDecryptTransform builds the "repository bytes -> live plaintext" step
// differ and applier read repository content through, or nil when no age
// key is configured - nil keeps differ.Compute's hard failure on sops
// ciphertext (its noAgeKeyReason) instead of applying ENC[...] as config.
// repoRoot is captured so sops can decrypt in place from repoRoot/rel.
func repoDecryptTransform(crypter *sopscrypt.Crypter, repoRoot string) differ.RepoTransform {
	if !crypter.Enabled() {
		return nil
	}
	return func(rel string, data []byte) ([]byte, bool, error) {
		if !sopscrypt.IsEncrypted(data) {
			return data, false, nil
		}
		// context.Background: neither seam carries a context in, and every
		// sops call is bounded by the Crypter's own timeout anyway.
		plain, err := crypter.DecryptFile(context.Background(), filepath.Join(repoRoot, rel))
		if err != nil {
			return nil, false, err
		}
		return plain, true, nil
	}
}

// applierRepoTransform adapts a repoDecryptTransform for internal/applier,
// dropping the "was it encrypted" flag only the differ needs.
func applierRepoTransform(transform differ.RepoTransform) applier.TransformRepoFileFunc {
	if transform == nil {
		return nil
	}
	return func(rel string, data []byte) ([]byte, error) {
		plain, _, err := transform(rel, data)
		return plain, err
	}
}
