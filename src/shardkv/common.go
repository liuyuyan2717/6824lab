package shardkv

import (
	"6.824/shardctrler"
	"fmt"
	"log"
	"time"
)

//
// Sharded key/value server.
// Lots of replica groups, each running op-at-a-time paxos.
// Shardctrler decides which group serves each shard.
// Shardctrler may change shard assignment from time to time.
//
// You will have to modify these definitions.
//

// Timeout constants for various operations in the sharded KV store
const (
	ExecuteTimeout            = 500 * time.Millisecond  // Timeout for command execution
	ConfigureMonitorTimeout   = 100 * time.Millisecond  // Interval for checking configuration changes
	MigrationMonitorTimeout   = 50 * time.Millisecond   // Interval for checking shard migration status
	GCMonitorTimeout          = 50 * time.Millisecond   // Interval for garbage collection of old shards
	EmptyEntryDetectorTimeout = 200 * time.Millisecond  // Interval for detecting empty log entries
)

const Debug = false // Enable debug logging when true

// DPrintf prints debug messages when Debug is enabled
func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug {
		log.Printf(format, a...)
	}
	return
}

// Err represents error codes returned by the sharded KV store
type Err uint8

const (
	OK Err = iota // Operation succeeded
	ErrNoKey      // Key does not exist
	ErrWrongGroup // Key belongs to a different shard group
	ErrWrongLeader // Request sent to non-leader replica
	ErrOutDated   // Configuration is outdated
	ErrTimeout    // Operation timed out
	ErrNotReady   // Shard is not ready to serve requests
)

func (err Err) String() string {
	switch err {
	case OK:
		return "OK"
	case ErrNoKey:
		return "ErrNoKey"
	case ErrWrongGroup:
		return "ErrWrongGroup"
	case ErrWrongLeader:
		return "ErrWrongLeader"
	case ErrOutDated:
		return "ErrOutDated"
	case ErrTimeout:
		return "ErrTimeout"
	case ErrNotReady:
		return "ErrNotReady"
	}
	panic(fmt.Sprintf("unexpected Err %d", err))
}

// ShardStatus represents the current state of a shard in the system
type ShardStatus uint8

const (
	Serving ShardStatus = iota  // Shard is actively serving requests
	Pulling                     // Shard is being pulled from another group
	BePulling                   // Shard is being pulled by another group (source side)
	GCing                       // Shard is ready for garbage collection after migration
)

func (status ShardStatus) String() string {
	switch status {
	case Serving:
		return "Serving"
	case Pulling:
		return "Pulling"
	case BePulling:
		return "BePulling"
	case GCing:
		return "GCing"
	}
	panic(fmt.Sprintf("unexpected ShardStatus %d", status))
}

// OperationContext tracks the latest operation from a client for deduplication
type OperationContext struct {
	MaxAppliedCommandId int64          // Highest command ID processed from this client
	LastResponse        *CommandResponse // Response of the last operation from this client
}

// deepCopy creates a deep copy of OperationContext to avoid aliasing issues
func (context OperationContext) deepCopy() OperationContext {
	return OperationContext{context.MaxAppliedCommandId, &CommandResponse{context.LastResponse.Err, context.LastResponse.Value}}
}

// Command represents an operation to be applied to the Raft log
type Command struct {
	Op   CommandType  // Type of command (operation, configuration change, etc.)
	Data interface{}  // Command-specific data
}

func (command Command) String() string {
	return fmt.Sprintf("{Type:%v,Data:%v}", command.Op, command.Data)
}

// NewOperationCommand creates a command for a client operation (Put/Append/Get)
func NewOperationCommand(request *CommandRequest) Command {
	return Command{Operation, *request}
}

// NewConfigurationCommand creates a command for a configuration update
func NewConfigurationCommand(config *shardctrler.Config) Command {
	return Command{Configuration, *config}
}

// NewInsertShardsCommand creates a command for inserting migrated shards
func NewInsertShardsCommand(response *ShardOperationResponse) Command {
	return Command{InsertShards, *response}
}

// NewDeleteShardsCommand creates a command for deleting migrated shards
func NewDeleteShardsCommand(request *ShardOperationRequest) Command {
	return Command{DeleteShards, *request}
}

// NewEmptyEntryCommand creates an empty command to advance the commit index
func NewEmptyEntryCommand() Command {
	return Command{EmptyEntry, nil}
}

// CommandType represents the type of command in the Raft log
type CommandType uint8

const (
	Operation CommandType = iota    // Client operation (Put/Append/Get)
	Configuration                   // Configuration change from shard controller
	InsertShards                    // Insert shards migrated from another group
	DeleteShards                    // Delete shards after migration to another group
	EmptyEntry                      // Empty entry to advance commit index
)

func (op CommandType) String() string {
	switch op {
	case Operation:
		return "Operation"
	case Configuration:
		return "Configuration"
	case InsertShards:
		return "InsertShards"
	case DeleteShards:
		return "DeleteShards"
	case EmptyEntry:
		return "EmptyEntry"
	}
	panic(fmt.Sprintf("unexpected CommandType %d", op))
}

// OperationOp represents the type of client operation
type OperationOp uint8

const (
	OpPut OperationOp = iota    // Put operation: set key to value
	OpAppend                    // Append operation: append value to existing key
	OpGet                       // Get operation: retrieve value for key
)

func (op OperationOp) String() string {
	switch op {
	case OpPut:
		return "OpPut"
	case OpAppend:
		return "OpAppend"
	case OpGet:
		return "OpGet"
	}
	panic(fmt.Sprintf("unexpected OperationOp %d", op))
}

// CommandRequest represents a client request for a key-value operation
type CommandRequest struct {
	Key       string      // Key to operate on
	Value     string      // Value for Put/Append operations
	Op        OperationOp // Type of operation (Put/Append/Get)
	ClientId  int64       // Unique client identifier
	CommandId int64       // Monotonically increasing command ID for deduplication
}

func (request CommandRequest) String() string {
	return fmt.Sprintf("Shard:%v,Key:%v,Value:%v,Op:%v,ClientId:%v,CommandId:%v}", key2shard(request.Key), request.Key, request.Value, request.Op, request.ClientId, request.CommandId)
}

// CommandResponse represents the response to a client request
type CommandResponse struct {
	Err   Err    // Error code indicating success or failure
	Value string // Value returned for Get operations
}

func (response CommandResponse) String() string {
	return fmt.Sprintf("{Err:%v,Value:%v}", response.Err, response.Value)
}

// ShardOperationRequest is used during shard migration to request shard data
type ShardOperationRequest struct {
	ConfigNum int   // Configuration number for consistency checking
	ShardIDs  []int // IDs of shards being requested
}

func (request ShardOperationRequest) String() string {
	return fmt.Sprintf("{ConfigNum:%v,ShardIDs:%v}", request.ConfigNum, request.ShardIDs)
}

// ShardOperationResponse contains shard data during migration
type ShardOperationResponse struct {
	Err            Err                          // Error code
	ConfigNum      int                          // Configuration number
	Shards         map[int]map[string]string    // Key-value data for each shard
	LastOperations map[int64]OperationContext   // Client operation history for deduplication
}

func (response ShardOperationResponse) String() string {
	return fmt.Sprintf("{Err:%v,ConfigNum:%v,ShardIDs:%v,LastOperations:%v}", response.Err, response.ConfigNum, response.Shards, response.LastOperations)
}
