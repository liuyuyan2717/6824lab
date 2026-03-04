package shardkv

//
// client code to talk to a sharded key/value service.
//
// the client first talks to the shardctrler to find out
// the assignment of shards (keys) to groups, and then
// talks to the group that holds the key's shard.
//

import "6.824/labrpc"
import "crypto/rand"
import "math/big"
import "6.824/shardctrler"
import "time"

// which shard is a key in?
// please use this function,
// and please do not change it.
func key2shard(key string) int {
	shard := 0
	if len(key) > 0 {
		shard = int(key[0])
	}
	shard %= shardctrler.NShards
	return shard
}

// nrand generates a random int64 for client ID generation
func nrand() int64 {
	max := big.NewInt(int64(1) << 62)
	bigx, _ := rand.Int(rand.Reader, max)
	return bigx.Int64()
}

// Clerk is the client interface to the sharded key-value store
type Clerk struct {
	sm        *shardctrler.Clerk           // Client to shard controller for configuration queries
	config    shardctrler.Config           // Cached configuration mapping shards to groups
	makeEnd   func(string) *labrpc.ClientEnd // Function to create RPC endpoints
	leaderIds map[int]int                  // Cached leader ID for each group (for efficiency)
	clientId  int64                        // Unique client ID for deduplication
	commandId int64                        // Monotonically increasing command ID (clientId, commandId) uniquely identifies each operation
}

// MakeClerk creates a new client for the sharded key-value store
//
// ctrlers[] is needed to call shardctrler.MakeClerk().
//
// make_end(servername) turns a server name from a
// Config.Groups[gid][i] into a labrpc.ClientEnd on which you can
// send RPCs.
func MakeClerk(ctrlers []*labrpc.ClientEnd, makeEnd func(string) *labrpc.ClientEnd) *Clerk {
	ck := &Clerk{
		sm:        shardctrler.MakeClerk(ctrlers), // Create shard controller client
		makeEnd:   makeEnd,                        // Store endpoint creation function
		leaderIds: make(map[int]int),              // Initialize leader cache
		clientId:  nrand(),                        // Generate random client ID
		commandId: 0,                              // Start command sequence at 0
	}
	ck.config = ck.sm.Query(-1) // Query latest configuration
	return ck
}

// Get retrieves the value for a key from the sharded KV store
func (ck *Clerk) Get(key string) string {
	return ck.Command(&CommandRequest{Key: key, Op: OpGet})
}

// Put sets the value for a key in the sharded KV store
func (ck *Clerk) Put(key string, value string) {
	ck.Command(&CommandRequest{Key: key, Value: value, Op: OpPut})
}

// Append appends a value to an existing key in the sharded KV store
func (ck *Clerk) Append(key string, value string) {
	ck.Command(&CommandRequest{Key: key, Value: value, Op: OpAppend})
}

// Command sends a request to the sharded KV store and handles retries
// This is the main method that implements the client-side logic for:
// 1. Determining which group owns the key's shard
// 2. Trying servers in the group to find the leader
// 3. Handling configuration changes when keys move between groups
func (ck *Clerk) Command(request *CommandRequest) string {
	request.ClientId, request.CommandId = ck.clientId, ck.commandId
	for {
		shard := key2shard(request.Key)
		gid := ck.config.Shards[shard]
		if servers, ok := ck.config.Groups[gid]; ok {
			// Initialize leader cache for this group if needed
			if _, ok = ck.leaderIds[gid]; !ok {
				ck.leaderIds[gid] = 0
			}
			oldLeaderId := ck.leaderIds[gid]
			newLeaderId := oldLeaderId
			// Try servers in the group to find the leader
			for {
				var response CommandResponse
				ok := ck.makeEnd(servers[newLeaderId]).Call("ShardKV.Command", request, &response)
				if ok && (response.Err == OK || response.Err == ErrNoKey) {
					// Success: increment command ID and return value
					ck.commandId++
					return response.Value
				} else if ok && response.Err == ErrWrongGroup {
					// Key has moved to a different group, break to update config
					break
				} else {
					// Try next server in the group (round-robin)
					newLeaderId = (newLeaderId + 1) % len(servers)
					if newLeaderId == oldLeaderId {
						// Tried all servers, break to update config
						break
					}
					continue
				}
			}
		}
		// If we get here, either the group doesn't exist or all servers failed
		// Wait and query for updated configuration
		time.Sleep(100 * time.Millisecond)
		// Query shard controller for latest configuration
		ck.config = ck.sm.Query(-1)
	}
}
