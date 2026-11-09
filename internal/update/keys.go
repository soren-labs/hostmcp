package update

// trustedReleaseKeys are the release-signing public keys this build accepts for
// automatic updates. The matching private keys are held by the release owner
// outside GitHub and CI (see docs/releasing.md).
//
// To rotate: add the new key here, publish a release signed with the old key,
// let the fleet update, then sign later releases with the new key and remove
// the old one. A node only learns new keys by installing a release it already
// trusts.
var trustedReleaseKeys = []string{
	// b01f7233bca74eac, created 2026-11-09 on the owner's Mac mini.
	"rmc-release-ed25519:shoAmTkzCXn/qQ1RjnbWNhYk3KDCy0GGqjOwzHgWMNM=",
}

// TrustedKeys parses the compiled-in release keys.
func TrustedKeys() ([]PublicKey, error) {
	keys := make([]PublicKey, 0, len(trustedReleaseKeys))
	for _, s := range trustedReleaseKeys {
		k, err := ParsePublicKey(s)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}
