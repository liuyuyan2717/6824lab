package shardkv

// Shard represents a single shard (partition) of the key-value store
// Each shard contains a subset of keys and has a status indicating its role in migration
type Shard struct {
	KV     map[string]string // Key-value storage for this shard
	Status ShardStatus       // Current status (Serving, Pulling, BePulling, GCing)
}

// NewShard creates a new shard with empty storage and Serving status
func NewShard() *Shard {
	return &Shard{make(map[string]string), Serving}
}

// Get retrieves a value from the shard
func (shard *Shard) Get(key string) (string, Err) {
	if value, ok := shard.KV[key]; ok {
		return value, OK
	}
	return "", ErrNoKey
}

// Put stores a key-value pair in the shard
func (shard *Shard) Put(key, value string) Err {
	shard.KV[key] = value
	return OK
}

// Append appends a value to an existing key in the shard
func (shard *Shard) Append(key, value string) Err {
	shard.KV[key] += value
	return OK
}

// deepCopy creates a deep copy of the shard's key-value data
// Used during shard migration to transfer data between groups
func (shard *Shard) deepCopy() map[string]string {
	newShard := make(map[string]string)
	for k, v := range shard.KV {
		newShard[k] = v
	}
	return newShard
}
