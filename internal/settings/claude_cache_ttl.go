package settings

// ClaudeCacheTTLConfig forces an hour only where the caller hasn't picked
// a lifetime. Groups match the requested model, keys match gateway key ids
// or names, and exclusions win over both lists.
type ClaudeCacheTTLConfig struct {
	TTL        string   `json:"ttl,omitempty"`
	Groups     []string `json:"groups,omitempty"`
	Keys       []string `json:"keys,omitempty"`
	ExceptKeys []string `json:"exceptKeys,omitempty"`
}
